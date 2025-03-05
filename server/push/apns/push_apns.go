package apns

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/sideshow/apns2"
	"github.com/sideshow/apns2/certificate"
	"github.com/tinode/chat/server/logs"
	"github.com/tinode/chat/server/push"
	"github.com/tinode/chat/server/push/common"
	"github.com/tinode/chat/server/store"
	"log"
)

var handler Handler

const (
	// Size of the input channel buffer.
	bufferSize = 1024

	// The number of sub/unsub requests sent in one batch. FCM constant.
	subBatchSize = 1000
)

type Handler struct {
	input     chan *push.Receipt
	channel   chan *push.ChannelReq
	stop      chan bool
	client    *apns2.Client
	devClient *apns2.Client
}

type configType struct {
	Enabled             bool           `json:"enabled"`
	CredentialsFile     string         `json:"credentials_file"`
	CredentialsPassword string         `json:"credentials_password"`
	AppTopic            string         `json:"app_topic"`
	TimeToLive          int            `json:"time_to_live,omitempty"`
	Env                 string         `json:"env"`
	CommonConfig        *common.Config `json:"common_config"`
}

func (h Handler) Init(jsonconf json.RawMessage) (bool, error) {
	var config configType
	err := json.Unmarshal([]byte(jsonconf), &config)
	if err != nil {
		return false, errors.New("failed to parse config: " + err.Error())
	}

	if !config.Enabled {
		return false, nil
	}

	fmt.Printf("hhhhh%+v\n", config)

	cert, err := certificate.FromP12File(config.CredentialsFile, config.CredentialsPassword)
	if err != nil {
		log.Fatal("Cert Error:", err)
	}

	if config.Env == "dev" {
		handler.client = apns2.NewClient(cert).Development() // TODO 上线要切换成线上环境
	} else {
		handler.client = apns2.NewClient(cert).Production()
		handler.devClient = apns2.NewClient(cert).Development()
	}

	handler.input = make(chan *push.Receipt, bufferSize)
	handler.channel = make(chan *push.ChannelReq, bufferSize)
	handler.stop = make(chan bool, 1)

	go func() {
		for {
			select {
			case rcpt := <-handler.input:
				go sendApns(rcpt, &config)
			case sub := <-handler.channel:
				fmt.Printf("fcm channel msg %+v\n", sub)
			case <-handler.stop:
				return
			}
		}
	}()

	return true, nil
}

func sendApns(rcpt *push.Receipt, config *configType) {
	messages, uids := PrepareApnsNotifications(rcpt, config)
	for i := range messages {
		notification := messages[i]

		test, _ := json.Marshal(notification)
		fmt.Printf("json encode notification: %s\n", test)
		fmt.Printf("%+v\n", notification)

		//If you want to test push notifications for builds running directly from XCode (Development), use
		//client := apns2.NewClient(cert).Development()
		//For apps published to the app store or installed as an ad-hoc distribution use Production()
		var res *apns2.Response
		var err error
		if uids[i].String() == "kNNdB09qcZI" || uids[i].String() == "b_6wGAmdDUY" {
			fmt.Printf("send push dev, uid: %s\n", uids[i].String())
			res, err = handler.devClient.Push(notification)
		} else {
			fmt.Printf("send push proc, uid: %s\n", uids[i].String())
			res, err = handler.client.Push(notification)
		}

		if err != nil {
			logs.Warn.Println("apns push err:", err)
			return
		}

		//fmt.Printf("%v %v %v\n", res.StatusCode, res.ApnsID, res.Reason)

		if res.StatusCode != 200 {
			switch res.Reason {
			case apns2.ReasonInternalServerError, apns2.ReasonServiceUnavailable:
				// Transient errors. Stop sending this batch.
				logs.Warn.Println("apns transient failure:", res.StatusCode, res.Reason)
				return
			case apns2.ReasonBadCollapseID, apns2.ReasonBadDeviceToken, apns2.ReasonBadExpirationDate, apns2.ReasonBadMessageID, apns2.ReasonBadPriority:
			case apns2.ReasonBadTopic, apns2.ReasonDeviceTokenNotForTopic, apns2.ReasonDuplicateHeaders, apns2.ReasonIdleTimeout, apns2.ReasonInvalidPushType:
			case apns2.ReasonMissingDeviceToken, apns2.ReasonMissingTopic, apns2.ReasonPayloadEmpty, apns2.ReasonTopicDisallowed, apns2.ReasonBadCertificate:
				// Config errors. Stop.
				logs.Warn.Println("apns invalid config:", res.StatusCode, res.Reason)
				return
			case apns2.ReasonUnregistered:
				// Token is no longer valid. Delete token from DB and continue sending.
				logs.Warn.Println("apns invalid token:", res.StatusCode, res.Reason)
				if err := store.Devices.Delete(uids[i], messages[i].DeviceToken); err != nil {
					logs.Warn.Println("apns failed to delete invalid token:", err)
				}
			default:
				// Unknown error. Stop sending just in case.
				logs.Warn.Println("apns unrecognized error:", res.StatusCode, res.Reason)
				return
			}
		}
	}
}

func (h Handler) IsReady() bool {
	return handler.input != nil
}

func (h Handler) Push() chan<- *push.Receipt {
	return handler.input
}

func (h Handler) Channel() chan<- *push.ChannelReq {
	return handler.channel
}

func (h Handler) Stop() {
	handler.stop <- true
}

func init() {
	push.Register("apns", &handler)
}
