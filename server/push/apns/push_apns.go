package apns

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/sideshow/apns2"
	"github.com/tinode/chat/server/push"
)

var handler Handler

const (
	// Size of the input channel buffer.
	bufferSize = 1024

	// The number of sub/unsub requests sent in one batch. FCM constant.
	subBatchSize = 1000
)

type Handler struct {
	input   chan *push.Receipt
	channel chan *push.ChannelReq
	stop    chan bool
	client  *apns2.Client
}

type configType struct {
	Enabled         bool   `json:"enabled"`
	CredentialsFile string `json:"credentials_file"`
	AppTopic        string `json:"app_topic"`
	TimeToLive      int    `json:"time_to_live,omitempty"`
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

	fmt.Println(jsonconf)

	//cert, err := certificate.FromP12File(config.CredentialsFile, "")
	//if err != nil {
	//	log.Fatal("Cert Error:", err)
	//}
	//
	//handler.client = apns2.NewClient(cert).Production()
	//
	//handler.input = make(chan *push.Receipt, bufferSize)
	//handler.channel = make(chan *push.ChannelReq, bufferSize)
	//handler.stop = make(chan bool, 1)
	//
	//go func() {
	//	for {
	//		select {
	//		//case rcpt := <-handler.input:
	//		//go sendApns(rcpt, &config)
	//		case sub := <-handler.channel:
	//			fmt.Printf("fcm channel msg %+v\n", sub)
	//		case <-handler.stop:
	//			return
	//		}
	//	}
	//}()
	//
	//notification := &apns2.Notification{}
	//notification.DeviceToken = "11aa01229f15f0f0c52029d8cf8cd0aeaf2365fe4cebc4af26cd6d76b7919ef7"
	//notification.Topic = "com.sideshow.Apns2"
	//notification.Payload = []byte(`{"aps":{"alert":"Hello!"}}`) // See Payload section below
	//
	//fmt.Printf("%+v", notification)
	////If you want to test push notifications for builds running directly from XCode (Development), use
	////client := apns2.NewClient(cert).Development()
	////For apps published to the app store or installed as an ad-hoc distribution use Production()
	//res, err := handler.client.Push(notification)
	//
	//if err != nil {
	//	log.Fatal("Error:", err)
	//}
	//
	//fmt.Printf("%v %v %v\n", res.StatusCode, res.ApnsID, res.Reason)

	return true, nil
}

//func sendApns(rcpt *push.Receipt, config *configType) {
//	messages, uids := fcm.PrepareV1Notifications(rcpt, config)
//	for i := range messages {
//		req := &fcmv1.SendMessageRequest{
//			Message:      messages[i],
//			ValidateOnly: config.DryRun,
//		}
//		_, err := handler.v1.Projects.Messages.Send("projects/"+handler.projectID, req).Do()
//		if err != nil {
//			gerr, decodingErrs := common.DecodeGoogleApiError(err)
//			for _, err := range decodingErrs {
//				logs.Info.Println("fcm googleapi.Error decoding:", err)
//			}
//			switch gerr.FcmErrCode {
//			case "": // no error
//			case common.ErrorQuotaExceeded, common.ErrorUnavailable, common.ErrorInternal, common.ErrorUnspecified:
//				// Transient errors. Stop sending this batch.
//				logs.Warn.Println("fcm transient failure:", gerr.FcmErrCode, gerr.ErrMessage)
//				return
//			case common.ErrorSenderIDMismatch, common.ErrorInvalidArgument, common.ErrorThirdPartyAuth:
//				// Config errors. Stop.
//				logs.Warn.Println("fcm invalid config:", gerr.FcmErrCode, gerr.ErrMessage)
//				return
//			case common.ErrorUnregistered:
//				// Token is no longer valid. Delete token from DB and continue sending.
//				logs.Warn.Println("fcm invalid token:", gerr.FcmErrCode, gerr.ErrMessage)
//				if err := store.Devices.Delete(uids[i], messages[i].Token); err != nil {
//					logs.Warn.Println("tnpg failed to delete invalid token:", err)
//				}
//			default:
//				// Unknown error. Stop sending just in case.
//				logs.Warn.Println("tnpg unrecognized error:", gerr.FcmErrCode, gerr.ErrMessage)
//				return
//			}
//		}
//	}
//}

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
