package apns

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/sideshow/apns2"
	"github.com/tinode/chat/server/push/common"
	"strconv"
	"strings"
	"time"

	"github.com/tinode/chat/server/drafty"
	"github.com/tinode/chat/server/logs"
	"github.com/tinode/chat/server/push"
	"github.com/tinode/chat/server/store"
	t "github.com/tinode/chat/server/store/types"
)

const (
	// TTL of a VOIP push notification in seconds.
	voipTimeToLive = 10
	// TTL of a regular push notification in seconds.
	defaultTimeToLive = 3600

	ACT_AUIDO  = 1
	ACT_VIDEO  = 2
	ACT_MISSED = 3
)

func payloadToData(pl *push.Payload) (map[string]string, error) {
	if pl == nil {
		return nil, errors.New("empty push payload")
	}
	data := make(map[string]string)
	var err error
	data["what"] = pl.What
	if pl.Silent {
		data["silent"] = "true"
	}

	data["topic"] = pl.Topic
	data["ts"] = pl.Timestamp.Format(time.RFC3339Nano)
	// Must use "xfrom" because "from" is a reserved word. Google did not bother to document it anywhere.
	data["xfrom"] = pl.From
	if pl.What == push.ActMsg {
		data["seq"] = strconv.Itoa(pl.SeqId)
		if pl.ContentType != "" {
			data["mime"] = pl.ContentType
		}

		// Convert Drafty content to plain text (clients 0.16 and below).
		data["content"], err = drafty.PlainText(pl.Content)

		switch t.GetTopicCat(pl.Topic) {
		case t.TopicCatP2P:
			data["title"] = getUserName(pl)
		case t.TopicCatGrp:
			fallthrough
		case t.TopicCatFnd:
			fallthrough
		case t.TopicCatSys:
			data["title"] = getTopicName(pl)
			data["content"] = fmt.Sprintf("%s: %s", getUserName(pl), data["content"])
		default:
		}

		if err != nil {
			return nil, err
		}
		// Trim long strings to 128 runes.
		// Check byte length first and don't waste time converting short strings.
		if len(data["content"]) > push.MaxPayloadLength {
			runes := []rune(data["content"])
			if len(runes) > push.MaxPayloadLength {
				data["content"] = string(runes[:push.MaxPayloadLength]) + "…"
			}
		}

		// Rich content for clients version 0.17 and above.
		data["rc"], err = drafty.Preview(pl.Content, push.MaxPayloadLength)

		if pl.Webrtc != "" {
			fmt.Printf("webrtc %v \n", pl.Webrtc)
			data["webrtc"] = pl.Webrtc
			if pl.AudioOnly {
				data["aonly"] = "true"
				data["content"] = "[AUDIO CALL]"
				data["act"] = strconv.Itoa(ACT_AUIDO)
			} else {
				data["content"] = "[VIDEO CALL]"
				data["act"] = strconv.Itoa(ACT_VIDEO)
			}
			// when Caller hang-up the call
			if pl.Webrtc == "missed" {
				data["content"] = "[MISSED CALL]"
				data["act"] = strconv.Itoa(ACT_MISSED)
			}
			// Video call push notifications are silent.
			data["silent"] = "true"
		}
		if pl.Replace != "" {
			// Notification of a message edit should be silent too.
			data["silent"] = "true"
			data["replace"] = pl.Replace
		}
		if err != nil {
			return nil, err
		}
	} else if pl.What == push.ActSub {
		data["modeWant"] = pl.ModeWant.String()
		data["modeGiven"] = pl.ModeGiven.String()
	} else if pl.What == push.ActRead {
		data["seq"] = strconv.Itoa(pl.SeqId)
		data["silent"] = "true"
	} else {
		return nil, errors.New("unknown push type")
	}
	return data, nil
}

func clonePayload(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src))
	for key, val := range src {
		dst[key] = val
	}
	return dst
}

func PrepareApnsNotifications(rcpt *push.Receipt, config *configType) ([]*apns2.Notification, []t.Uid) {
	data, err := payloadToData(&rcpt.Payload)
	if err != nil {
		logs.Warn.Println("apns push: could not parse payload:", err)
		return nil, nil
	}

	// Device IDs to send pushes to.
	var devices map[t.Uid][]t.DeviceDef
	// Count of device IDs to push to.
	var count int
	// Devices which were online in the topic when the message was sent.
	skipDevices := make(map[string]struct{})
	if len(rcpt.To) > 0 {
		// List of UIDs for querying the database

		uids := make([]t.Uid, len(rcpt.To))
		i := 0
		for uid, to := range rcpt.To {
			uids[i] = uid
			i++
			// Some devices were online and received the message. Skip them.
			for _, deviceID := range to.Devices {
				skipDevices[deviceID] = struct{}{}
			}
		}
		devices, count, err = store.Devices.GetAll(uids...)
		if err != nil {
			logs.Warn.Println("apns push: db error", err)
			return nil, nil
		}
	}
	if count == 0 && rcpt.Channel == "" {
		return nil, nil
	}

	if config == nil {
		// config is nil when called from tnpg adapter; provide a blank one for simplicity.
		config = &configType{}
	}

	var messages []*apns2.Notification
	var uids []t.Uid
	for uid, devList := range devices {
		topic := rcpt.Payload.Topic
		userData := data
		tcat := t.GetTopicCat(topic)
		if rcpt.To[uid].Delivered > 0 || tcat == t.TopicCatP2P {
			userData = clonePayload(data)
			// Fix topic name for P2P pushes.
			if tcat == t.TopicCatP2P {
				topic, _ = t.P2PNameForUser(uid, topic)
				userData["topic"] = topic
			}
			// Silence the push for user who have received the data interactively.
			if rcpt.To[uid].Delivered > 0 {
				userData["silent"] = "true"
			}
		}

		for i := range devList {
			d := &devList[i]
			if _, ok := skipDevices[d.DeviceId]; !ok && d.DeviceId != "" {
				msg := apns2.Notification{
					DeviceToken: d.DeviceId,
					Topic:       config.AppTopic,
					CollapseID:  topic,
				}

				switch d.Platform {
				case "ios":
					msg, err = apnsNotificationConfig(rcpt.Payload.What, topic, userData, rcpt.To[uid].Unread, config, msg, uid)
					if err != nil {
						logs.Warn.Println("apns: generate notification config err", err)
					}
				case "web":
				case "":
					// ignore
				default:
					logs.Warn.Println("apns: unknown device platform", d.Platform)
				}

				uids = append(uids, uid)
				messages = append(messages, &msg)
			}
		}
	}

	return messages, uids
}

// DevicesForUser loads device IDs of the given user.
func DevicesForUser(uid t.Uid) []string {
	ddef, count, err := store.Devices.GetAll(uid)
	if err != nil {
		logs.Warn.Println("fcm devices for user: db error", err)
		return nil
	}

	if count == 0 {
		return nil
	}

	devices := make([]string, count)
	for i, dd := range ddef[uid] {
		devices[i] = dd.DeviceId
	}
	return devices
}

// ChannelsForUser loads user's channel subscriptions with P permission.
func ChannelsForUser(uid t.Uid) []string {
	channels, err := store.Users.GetChannels(uid)
	if err != nil {
		logs.Warn.Println("fcm channels for user: db error", err)
		return nil
	}
	return channels
}

func apnsShouldPresentAlert(what, callStatus, isSilent string, config *configType) bool {
	return config.Enabled && what != push.ActRead && ((callStatus == "" && isSilent == "") || (callStatus == "started" || callStatus == "missed"))
}

func apnsNotificationConfig(what, topic string, data map[string]string, unread int, config *configType, msg apns2.Notification, uid t.Uid) (apns2.Notification, error) {
	callStatus := data["webrtc"]
	expires := time.Now().UTC().Add(time.Duration(defaultTimeToLive) * time.Second)
	if config.TimeToLive > 0 {
		expires = time.Now().UTC().Add(time.Duration(config.TimeToLive) * time.Second)
	}
	pushType := apns2.PushTypeAlert
	priority := 10
	interruptionLevel := common.InterruptionLevelTimeSensitive
	if callStatus == "started" || callStatus == "missed" {
		// Send VOIP push only when a new call is started, otherwise send normal alert.
		interruptionLevel = common.InterruptionLevelCritical
		// FIXME: PushKit notifications do not work with the current FCM adapter.
		// Using normal pushes as a poor-man's replacement for VOIP pushes.
		// Uncomment the following two lines when FCM fixes its problem or when we switch to
		// a different adapter.
		// TODO:: why push voip type, return DeviceTokenNotForTopic error
		//pushType = apns2.PushTypeVOIP
		//msg.Topic += ".voip"
		expires = time.Now().UTC().Add(time.Duration(voipTimeToLive) * time.Second)
	} else if what == push.ActRead {
		priority = 5
		interruptionLevel = common.InterruptionLevelPassive
		pushType = apns2.PushTypeBackground
	}

	sound := "default"
	// TODO when testing account call, push incoming sound notify
	//if callStatus == "started" && (uid.String() == "kNNdB09qcZI" || uid.String() == "b_6wGAmdDUY") {
	//	sound = "incoming.wav"
	//}

	apsPayload := common.Aps{
		Badge:             unread,
		ContentAvailable:  1,
		MutableContent:    1,
		InterruptionLevel: interruptionLevel,
		Sound:             sound,
		ThreadID:          topic,
	}

	// Do not present alert for read notifications and video calls.
	if apnsShouldPresentAlert(what, callStatus, data["silent"], config) {
		body := config.CommonConfig.GetStringField(what, "Body")
		if body == "$content" {
			body = data["content"]
		}
		title := config.CommonConfig.GetStringField(what, "Title")
		if title == "$title" {
			title = data["title"]
		}

		apsPayload.Alert = &common.ApsAlert{
			Action:          config.CommonConfig.GetStringField(what, "Action"),
			ActionLocKey:    config.CommonConfig.GetStringField(what, "ActionLocKey"),
			Body:            body,
			LaunchImage:     config.CommonConfig.GetStringField(what, "LaunchImage"),
			LocKey:          config.CommonConfig.GetStringField(what, "LocKey"),
			Title:           title,
			Subtitle:        config.CommonConfig.GetStringField(what, "Subtitle"),
			TitleLocKey:     config.CommonConfig.GetStringField(what, "TitleLocKey"),
			SummaryArg:      config.CommonConfig.GetStringField(what, "SummaryArg"),
			SummaryArgCount: config.CommonConfig.GetIntField(what, "SummaryArgCount"),
		}
	}

	fmt.Printf("apspayload: %+v\n", apsPayload.Alert)

	var tmpPayload map[string]interface{}

	if callStatus == "started" || callStatus == "missed" {
		tmpPayload = map[string]interface{}{"aps": apsPayload, "act": data["act"]}
	} else {
		tmpPayload = map[string]interface{}{"aps": apsPayload}
	}

	payload, err := json.Marshal(tmpPayload)

	if err != nil {
		return msg, err
	}

	msg.CollapseID = topic
	msg.Expiration = expires
	msg.PushType = pushType
	msg.Priority = priority
	msg.Payload = payload

	return msg, nil
}

// get username from payload.From
func getUserName(pl *push.Payload) string {
	var userPublic interface{}
	username := ""
	if pl.FromPub != nil {
		userPublic = pl.FromPub
	} else {
		uid := t.ParseUserId(pl.From)

		if uid.IsZero() {
			logs.Warn.Println("apns parse uid.IsZero")
			return ""
		}

		suser, err := store.Users.Get(uid)
		if err != nil {
			logs.Warn.Println("apns get user error: ", err)
			return ""
		}
		if suser == nil {
			logs.Warn.Println("apns user not found")
			return ""
		}
		userPublic = suser.Public
	}

	if userInfo, ok := userPublic.(map[string]interface{}); ok {
		if username, ok = userInfo["fn"].(string); !ok {
			logs.Warn.Println("apns parse user info fail")
			return ""
		}
	}

	return username
}

// get topic from Payload.Topic
func getTopicName(pl *push.Payload) string {
	var topicPublic interface{}
	topicName := ""
	if pl.TopicPub != nil {
		topicPublic = pl.TopicPub
	} else {
		stopic, err := store.Topics.Get(pl.Topic)
		if err != nil {
			logs.Warn.Println("apns get topic info error: ", err)
			return ""
		}
		if stopic == nil {
			logs.Warn.Println("apns topic not found")
			return ""
		}
		topicPublic = stopic.Public
	}

	if topicInfo, ok := topicPublic.(map[string]interface{}); ok {
		if topicName, ok = topicInfo["fn"].(string); !ok {
			logs.Warn.Println("apns parse topic info fail")
			return ""
		}
	}

	return topicName
}

func getTitle(pl *push.Payload) string {
	notifyTitle := ""

	if strings.HasPrefix(pl.Topic, "grp") || pl.Topic == "sys" {
		stopic, err := store.Topics.Get(pl.Topic)
		if err != nil {
			logs.Warn.Println("apns get topic info error: ", err)
			return ""
		}
		if stopic == nil {
			logs.Warn.Println("apns topic not found")
			return ""
		}

		if topicInfo, ok := stopic.Public.(map[string]interface{}); ok {
			if notifyTitle, ok = topicInfo["fn"].(string); !ok {
				logs.Warn.Println("apns parse topic info error: ", err)
				return ""
			}
		}
	} else {
		// 'me' and p2p topics
		uid := t.ZeroUid
		if strings.HasPrefix(pl.Topic, "usr") {
			// User specified as usrXXX
			uid = t.ParseUserId(pl.Topic)
		} else if strings.HasPrefix(pl.Topic, "p2p") {
			uid = t.ParseUserId(pl.From)
		}

		if uid.IsZero() {
			logs.Warn.Println("apns parse uid.IsZero")
			return ""
		}

		suser, err := store.Users.Get(uid)
		if err != nil {
			logs.Warn.Println("apns get user error: ", err)
			return ""
		}
		if suser == nil {
			logs.Warn.Println("apns user not found")
			return ""
		}

		if userInfo, ok := suser.Public.(map[string]interface{}); ok {
			if notifyTitle, ok = userInfo["fn"].(string); !ok {
				logs.Warn.Println("apns parse user info error: ")
				return ""
			}
		}
	}

	if notifyTitle == "" {
		return "New message"
	}

	return notifyTitle
}
