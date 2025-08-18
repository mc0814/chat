package feishu

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"sync"
	"time"

	"github.com/tinode/chat/server/logs"
	"github.com/tinode/chat/server/push"
	"github.com/tinode/chat/server/store"
	t "github.com/tinode/chat/server/store/types"
)

var handler Handler

var tokenLock TokenLock

const (
	// Size of the input channel buffer.
	bufferSize = 1024

	// Tenant access token URL
	tenantAccessTokenURL = "https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal"

	// Message push URL
	messagePushURL = "https://open.feishu.cn/open-apis/im/v1/messages"

	// Urgent app message push URL
	urgentAppMessagePushURL = "https://open.feishu.cn/open-apis/im/v1/messages"
)

type Content struct {
	Tag  string `json:"tag"`
	Text string `json:"text,omitempty"`
}

type ContentList struct {
	ZhCn struct {
		Title   string      `json:"title"`
		Content [][]Content `json:"content"`
	} `json:"zh_cn"`
}

type TokenLock struct {
	mu sync.RWMutex
}

// Handler handles Feishu push notifications
type Handler struct {
	input      chan *push.Receipt
	channel    chan *push.ChannelReq
	stop       chan bool
	config     *configType
	tokenInfo  map[string]tenantAccessTokenInfo
	httpClient *http.Client
}

type configType struct {
	Enabled bool                   `json:"enabled"`
	AppList map[string]t.FeishuApp `json:"app_list"`
}

type tenantAccessTokenInfo struct {
	TenantAccessToken string `json:"tenant_access_token"`
	Expire            int    `json:"expire"`
	Timestamp         int64  `json:"timestamp"`
}

type feishuUser struct {
	unionId     string
	feishuAppId string
}

// Init initializes the Feishu push handler
func (h Handler) Init(jsonconf json.RawMessage) (bool, error) {
	var config configType
	err := json.Unmarshal([]byte(jsonconf), &config)
	if err != nil {
		return false, errors.New("failed to parse config: " + err.Error())
	}

	if !config.Enabled {
		return false, nil
	}

	config.AppList = make(map[string]t.FeishuApp)

	// Init feishu app
	feishuApps, err := store.FeishuApps.GetAll()
	for _, feishuApp := range feishuApps {
		config.AppList[feishuApp.AppId] = feishuApp
	}

	handler.config = &config
	handler.input = make(chan *push.Receipt, bufferSize)
	handler.channel = make(chan *push.ChannelReq, bufferSize)
	handler.stop = make(chan bool, 1)
	handler.httpClient = &http.Client{
		Timeout: 10 * time.Second,
	}
	handler.tokenInfo = make(map[string]tenantAccessTokenInfo)

	// Initialize token
	for _, feishuApp := range handler.config.AppList {
		if err := refreshTenantAccessToken(feishuApp.AppId, feishuApp.AppSecret); err != nil {
			logs.Warn.Println("Failed to initialize tenant access token:", err, feishuApp.AppId)
			continue
		}
	}

	// Start token refresher
	// go h.tokenRefresher()

	// Start message processor
	go processMessages()

	return true, nil
}

// refreshTenantAccessToken refresh tenant access token
func refreshTenantAccessToken(appId string, appSecret string) error {
	tokenLock.mu.Lock()
	defer tokenLock.mu.Unlock()

	// Prepare request body
	body := map[string]string{
		"app_id":     appId,
		"app_secret": appSecret,
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return err
	}

	// Send request
	req, err := http.NewRequest("POST", tenantAccessTokenURL, nil)
	if err != nil {
		return err
	}
	req.Body = ioutil.NopCloser(bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")

	resp, err := handler.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Parse response
	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var result struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return err
	}

	if result.Code != 0 {
		return fmt.Errorf("failed to get tenant_access_token: code=%d, msg=%s", result.Code, result.Msg)
	}

	// Update token info
	handler.tokenInfo[appId] = tenantAccessTokenInfo{
		TenantAccessToken: result.TenantAccessToken,
		Expire:            result.Expire,
		Timestamp:         time.Now().Unix(),
	}

	logs.Info.Println("Feishu tenant access token refreshed successfully", result, appId)
	return nil
}

// tokenRefresher timer to refresh tenant access token
// func (h Handler) tokenRefresher() {
// 	ticker := time.NewTicker(time.Hour)
// 	defer ticker.Stop()

// 	for {
// 		select {
// 		case <-ticker.C:
// 			h.mu.RLock()
// 			expireTime := h.tokenInfo.Timestamp + int64(h.tokenInfo.Expire) - 300 // Refresh 5 minutes before expiration
// 			h.mu.RUnlock()

// 			if time.Now().Unix() >= expireTime {
// 				if err := refreshTenantAccessToken(); err != nil {
// 					logs.Warn.Println("Failed to refresh tenant access token:", err)
// 				}
// 			}
// 		case <-h.stop:
// 			return
// 		}
// 	}
// }

// processMessages handle message
func processMessages() {
	for {
		select {
		case rcpt := <-handler.input:
			go sendFeishuMessage(rcpt)
		case sub := <-handler.channel:
			logs.Info.Printf("Feishu channel request: %+v\n", sub)
		case <-handler.stop:
			return
		}
	}
}

// getTenantAccessToken
func getTenantAccessToken(appId string) (token string, err error) {
	// check token expire
	tokenLock.mu.RLock()
	expireTime := handler.tokenInfo[appId].Timestamp + int64(handler.tokenInfo[appId].Expire) - 300
	tokenLock.mu.RUnlock()

	if time.Now().Unix() >= expireTime {
		if err = refreshTenantAccessToken(appId, handler.config.AppList[appId].AppSecret); err != nil {
			logs.Warn.Println("Failed to refresh tenant access token before sending message:", err)
			return token, err
		}
	}

	return handler.tokenInfo[appId].TenantAccessToken, nil
}

// sendFeishuMessage
func sendFeishuMessage(rcpt *push.Receipt) {
	// just push message
	if rcpt.Payload.What != push.ActMsg {
		return
	}

	// just push webrtc started
	if rcpt.Payload.Webrtc != "" && rcpt.Payload.Webrtc != "started" {
		return
	}

	// get user union_id
	var users []t.User
	var err error
	if len(rcpt.To) > 0 {
		fromUid := t.ParseUserId(rcpt.Payload.From)
		// List of UIDs for querying the database
		uids := make([]t.Uid, len(rcpt.To))
		i := 0
		for uid, _ := range rcpt.To {
			// skip user from message
			if uid == fromUid {
				continue
			}
			uids[i] = uid
			i++
		}

		users, err = store.Users.GetAll(uids...)
		if err != nil {
			logs.Warn.Println("feishu push: db error", err)
			return
		}
	}

	var feishuUsers []feishuUser
	for _, user := range users {
		feishuUsers = append(feishuUsers, feishuUser{
			unionId:     user.UnionId,
			feishuAppId: user.FeishuAppId,
		})
	}

	if len(feishuUsers) == 0 {
		return
	}

	// build message
	var messageText string
	var isUrgent bool

	// if message is webrtc, should urgent the message
	if rcpt.Payload.Webrtc != "" {
		// audio
		if rcpt.Payload.AudioOnly {
			messageText = "有人给你打音频通话，快打开软件看看吧"
		} else {
			messageText = "有人给你打视频通话，快打开软件看看吧"
		}
		isUrgent = true
	} else {
		// message
		messageText = "收到一条新消息，快打开软件看看吧"
		isUrgent = false
	}

	msgContent := ContentList{
		ZhCn: struct {
			Title   string      `json:"title"`
			Content [][]Content `json:"content"`
		}{
			Title: "IM",
			Content: [][]Content{
				{
					{Tag: "text", Text: messageText},
				},
			},
		},
	}

	msgContentJson, err := json.Marshal(msgContent)
	if err != nil {
		logs.Warn.Println("Failed to marshal message content:", err)
		return
	}

	// 发送消息给每个用户
	for _, feishuUser := range feishuUsers {
		sendMessage("union_id", feishuUser, string(msgContentJson), isUrgent)
	}
}

// sendSingleMessage
func sendMessage(receiveIdType string, sendUser feishuUser, content string, urgent bool) {
	// if app_id empty, skip
	if sendUser.feishuAppId == "" {
		return
	}

	token, err := getTenantAccessToken(sendUser.feishuAppId)
	if err != nil {
		logs.Warn.Println("Failed to get tenantAccessToken:", err)
		return
	}

	// message struct
	requestBody := map[string]interface{}{
		"receive_id": sendUser.unionId,
		"msg_type":   "post",
		"content":    content,
	}

	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		logs.Warn.Println("Failed to marshal message content:", err)
		return
	}

	url := fmt.Sprintf("%s?receive_id_type=%s", messagePushURL, receiveIdType)

	// 发送请求
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBuffer(jsonBody))
	if err != nil {
		logs.Warn.Println("Failed to create request:", err)
		return
	}

	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))

	logs.Warn.Println("feishu header", requestBody)

	resp, err := handler.httpClient.Do(req)
	if err != nil {
		logs.Warn.Println("Failed to send message:", err)
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		logs.Warn.Println("Failed to read response:", err)
		return
	}

	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			MessageId string `json:"message_id"`
		} `json:"data"`
		error struct {
			Message string `json:"message"`
		}
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		logs.Warn.Println("Failed to parse response:", err)
		return
	}

	if result.Code != 0 {
		logs.Warn.Println(result)
		logs.Warn.Printf("Failed to send message to %s: code=%d, msg=%s, token=%s, app_id=%s\n", sendUser.unionId, result.Code, result.Msg, token, sendUser.feishuAppId)
		return
	}

	logs.Info.Printf("Message sent successfully to %s, message_id: %s, app_id=%s\n", sendUser.unionId, result.Data.MessageId, sendUser.feishuAppId)

	if urgent {
		sendUrgentMessage(receiveIdType, sendUser, result.Data.MessageId)
	}
}

// sendUrgentMessage send urgent message to feishu
func sendUrgentMessage(receiveIdType string, sendUser feishuUser, messageId string) {
	token, err := getTenantAccessToken(sendUser.feishuAppId)
	if err != nil {
		logs.Warn.Println("Failed to get tenantAccessToken:", err)
		return
	}

	// message struct
	requestBody := map[string]interface{}{
		"user_id_list": []string{sendUser.unionId},
	}

	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		logs.Warn.Println("Failed to marshal urgent app message content:", err)
		return
	}

	url := fmt.Sprintf("%s/%s/urgent_app?user_id_type=%s", urgentAppMessagePushURL, messageId, receiveIdType)

	// 发送请求
	req, err := http.NewRequest(http.MethodPatch, url, bytes.NewBuffer(jsonBody))
	if err != nil {
		logs.Warn.Println("Failed to create urgent app message request:", err)
		return
	}

	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))

	resp, err := handler.httpClient.Do(req)
	if err != nil {
		logs.Warn.Println("Failed to send urgent app message:", err)
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		logs.Warn.Println("Failed to read urgent app message response:", err)
		return
	}

	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			MessageId string `json:"message_id"`
		} `json:"data"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		logs.Warn.Println("Failed to parse urgent app message response:", err)
		return
	}

	if result.Code != 0 {
		logs.Warn.Printf("Failed to send urgent app message to %s: code=%d, msg=%s, app_id=%s\n", sendUser.unionId, result.Code, result.Msg, sendUser.feishuAppId)
		return
	}

	logs.Info.Printf("Urgent app message sent successfully to %s, message_id: %s, app_id=%s\n", sendUser.unionId, result.Data.MessageId, sendUser.feishuAppId)
}

// IsReady checks if the handler is ready to process push notifications
func (h Handler) IsReady() bool {
	return handler.input != nil
}

// Push returns the channel for sending push notifications
func (h Handler) Push() chan<- *push.Receipt {
	return handler.input
}

// Channel returns the channel for sending channel requests
func (h Handler) Channel() chan<- *push.ChannelReq {
	return handler.channel
}

// Stop stops the handler
func (h Handler) Stop() {
	handler.stop <- true
}

func init() {
	push.Register("feishu", &handler)
}
