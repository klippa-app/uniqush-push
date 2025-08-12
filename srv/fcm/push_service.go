package fcm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"firebase.google.com/go/v4/errorutils"
	"firebase.google.com/go/v4/messaging"

	"github.com/appleboy/go-fcm"
	"github.com/uniqush/uniqush-push/push"
)

type pushService struct {
	errChan chan<- push.Error
}

var _ push.PushServiceType = &pushService{}

var ClientKey = "FCMClientKey"

func NewPushService() push.PushServiceType {

	return &pushService{}
}

func (ps *pushService) Name() string {
	return "fcm"
}

func (ps *pushService) Finalize() {

}

func (ps *pushService) SetErrorReportChan(errChan chan<- push.Error) {
	ps.errChan = errChan
}

// SetPushServiceConfig sets the config for this and the requestProcessor when the service is registered.
func (ps *pushService) SetPushServiceConfig(c *push.PushServiceConfig) {
}

func (ps *pushService) BuildPushServiceProviderFromMap(kv map[string]string, psp *push.PushServiceProvider) error {
	if service, ok := kv["service"]; ok {
		psp.FixedData["service"] = service
	} else {
		return errors.New("NoService")
	}

	if credentialsFile, ok := kv["credentialsfile"]; ok && len(credentialsFile) > 0 {
		psp.FixedData["credentialsfile"] = credentialsFile
	} else {
		return errors.New("NoCredentialsFile")
	}

	client, err := fcm.NewClient(
		context.Background(),
		fcm.WithCredentialsFile(psp.FixedData["credentialsfile"]),
	)
	if err != nil {
		return fmt.Errorf("could not initialize FCM client: %w", err)
	}

	psp.Data[ClientKey] = client

	return nil
}

func (ps *pushService) BuildDeliveryPointFromMap(kv map[string]string, dp *push.DeliveryPoint) error {
	err := dp.AddCommonData(kv)
	if err != nil {
		return err
	}
	if account, ok := kv["account"]; ok && len(account) > 0 {
		dp.FixedData["account"] = account
	}
	if regid, ok := kv["regid"]; ok && len(regid) > 0 {
		dp.FixedData["regid"] = regid
	} else {
		return errors.New("NoRegId")
	}

	return nil
}

// ToPayload will serialize notif as a push service payload
func (ps *pushService) ToPayload(notif *push.Notification, regIds []string) (*messaging.MulticastMessage, push.Error) {
	postData := notif.Data
	payload := new(messaging.MulticastMessage)
	payload.Tokens = regIds
	payload.Android = &messaging.AndroidConfig{}

	// TTL: default is one hour
	ttl := time.Second * (60 * 60)

	// This option is gone in the new API.
	// payload.DelayWhileIdle = false

	if mgroup, ok := postData["msggroup"]; ok {
		payload.Android.CollapseKey = mgroup
	} else {
		payload.Android.CollapseKey = ""
	}

	if ttlStr, ok := postData["ttl"]; ok {
		parsedTtl, err := strconv.ParseUint(ttlStr, 10, 32)
		if err == nil {
			ttl = time.Second * time.Duration(parsedTtl)
		}
	}

	payload.Android.TTL = &ttl

	// Support uniqush.notification.gcm/fcm as another optional payload, to conform to GCM spec: https://developers.google.com/cloud-messaging/http-server-ref#send-downstream
	// This will make GCM handle displaying the notification instead of the client.
	// Note that "data" payloads (Uniqush's default for GCM/FCM, for historic reasons) can be sent alongside "notification" payloads.
	// - In that case, "data" will be sent to the app, and "notification" will be rendered directly for the user to see.
	if rawNotification, ok := postData["uniqush.notification.fcm"]; ok {
		data := &messaging.Notification{}
		err := json.Unmarshal([]byte(rawNotification), data)
		if err != nil {
			return nil, push.NewErrorf("Could not decode JSON notification data: %v", err)
		}
		payload.Notification = data
	}
	if rawData, ok := postData["uniqush.payload.fcm"]; ok {
		data := map[string]string{}
		err := json.Unmarshal([]byte(rawData), &data)
		if err != nil {
			return nil, push.NewErrorf("Could not decode JSON payload data: %v", err)
		}
		payload.Data = data
	} else {
		payload.Data = make(map[string]string, len(postData))

		for k, v := range postData {
			if strings.HasPrefix(k, "uniqush.") { // The "uniqush." keys are reserved for uniqush use.
				continue
			}
			switch k {
			case "msggroup", "ttl", "collapse_key":
				continue
			default:
				payload.Data[k] = v
			}
		}
	}

	return payload, nil
}

func (ps *pushService) Preview(notif *push.Notification) ([]byte, push.Error) {
	payload, err := ps.ToPayload(notif, []string{"placeholderRegId"})
	if err != nil {
		return nil, err
	}

	jpayload, e0 := json.Marshal(payload)
	if e0 != nil {
		return nil, push.NewErrorf("Error converting payload to JSON: %v", err)
	}
	return jpayload, nil
}

// Push will read all of the delivery points to send to from dpQueue and send responses on resQueue before closing the channel. If the notification data is invalid,
// it will send only one response.
func (ps *pushService) Push(psp *push.PushServiceProvider, dpQueue <-chan *push.DeliveryPoint, resQueue chan<- *push.Result, notif *push.Notification) {
	maxNrDst := 500
	dpList := make([]*push.DeliveryPoint, 0, maxNrDst)
	for dp := range dpQueue {
		if psp.PushServiceName() != dp.PushServiceName() || psp.PushServiceName() != "fcm" {
			res := new(push.Result)
			res.Provider = psp
			res.Destination = dp
			res.Content = notif
			res.Err = push.NewIncompatibleError()
			resQueue <- res
			continue
		}
		if _, ok := dp.VolatileData["regid"]; ok {
			dpList = append(dpList, dp)
		} else if regid, ok := dp.FixedData["regid"]; ok {
			dp.VolatileData["regid"] = regid
			dpList = append(dpList, dp)
		} else {
			res := new(push.Result)
			res.Provider = psp
			res.Destination = dp
			res.Content = notif
			res.Err = push.NewBadDeliveryPointWithDetails(dp, fmt.Sprintf("uniqush delivery point for FCM is missing regid"))
			resQueue <- res
			continue
		}

		if len(dpList) >= maxNrDst {
			ps.multicast(psp, dpList, resQueue, notif)
			dpList = dpList[:0]
		}
	}
	if len(dpList) > 0 {
		ps.multicast(psp, dpList, resQueue, notif)
	}

	close(resQueue)
}

func extractRegIds(dpList []*push.DeliveryPoint) []string {
	regIds := make([]string, 0, len(dpList))

	for _, dp := range dpList {
		regIds = append(regIds, dp.VolatileData["regid"])
	}
	return regIds
}

func sendErrToEachDP(psp *push.PushServiceProvider, dpList []*push.DeliveryPoint, resQueue chan<- *push.Result, notif *push.Notification, err push.Error) {
	for _, dp := range dpList {
		res := new(push.Result)
		res.Provider = psp
		res.Content = notif

		res.Err = err
		res.Destination = dp
		resQueue <- res
	}
}

func (psb *pushService) handleMulticastResults(psp *push.PushServiceProvider, dpList []*push.DeliveryPoint, resQueue chan<- *push.Result, notif *push.Notification, results []*messaging.SendResponse) {
	for i, r := range results {
		if i >= len(dpList) {
			break
		}
		dp := dpList[i]
		if !r.Success {
			if r.Error != nil {
				errmsg := r.Error.Error()

				switch {
				case errorutils.IsResourceExhausted(r.Error),
					errorutils.IsUnavailable(r.Error),
					errorutils.IsInternal(r.Error):
					after, _ := time.ParseDuration("2s")
					res := new(push.Result)
					res.Provider = psp
					res.Content = notif
					res.Destination = dp
					res.Err = push.NewRetryError(psp, dp, notif, after)
					resQueue <- res
				case errorutils.IsNotFound(r.Error):
					res := new(push.Result)
					res.Provider = psp
					res.Err = push.NewUnsubscribeUpdate(psp, dp)
					res.Content = notif
					res.Destination = dp
					resQueue <- res
				default:
					res := new(push.Result)
					res.Err = push.NewErrorf("FCMError: %v", errmsg)
					res.Provider = psp
					res.Content = notif
					res.Destination = dp
					resQueue <- res
				}
			} else {
				res := new(push.Result)
				res.Err = push.NewErrorf("Unkown FCM message failure")
				res.Provider = psp
				res.Content = notif
				res.Destination = dp
				resQueue <- res
			}
		} else {
			res := new(push.Result)
			res.Provider = psp
			res.Content = notif
			res.Destination = dp
			res.MsgID = fmt.Sprintf("%v:%v", psp.Name(), r.MessageID)
			resQueue <- res
		}
	}
}

func (ps *pushService) multicast(psp *push.PushServiceProvider, dpList []*push.DeliveryPoint, resQueue chan<- *push.Result, notif *push.Notification) {

	psp.Lock.Lock()

	if psp.Data[ClientKey] == nil {
		client, err := fcm.NewClient(
			context.Background(),
			fcm.WithCredentialsFile(psp.FixedData["credentialsfile"]),
		)
		if err != nil {
			sendErrToEachDP(psp, dpList, resQueue, notif, push.NewErrorf("could not initialize FCM client: %v", err))
			return
		}

		psp.Data[ClientKey] = client
	}

	psp.Lock.Unlock()

	if len(dpList) == 0 {
		return
	}
	regIds := extractRegIds(dpList)

	payload, e0 := ps.ToPayload(notif, regIds)
	if e0 != nil {
		sendErrToEachDP(psp, dpList, resQueue, notif, e0)
		return
	}

	resp, err := psp.Data[ClientKey].(*fcm.Client).SendMulticast(context.Background(), payload)
	// TODO: Move this into two steps: sending and processing result
	if err != nil {
		for _, dp := range dpList {
			res := new(push.Result)
			res.Provider = psp
			res.Content = notif

			res.Destination = dp
			if err, ok := err.(net.Error); ok {
				// Temporary error. Try to recover
				if err.Temporary() {
					after := 3 * time.Second
					res.Err = push.NewRetryErrorWithReason(psp, dp, notif, after, err)
				}
			} else if err, ok := err.(*net.DNSError); ok {
				// DNS error, try to recover it by retry
				after := 3 * time.Second
				res.Err = push.NewRetryErrorWithReason(psp, dp, notif, after, err)

			} else {
				res.Err = push.NewErrorf("Unrecoverable HTTP error sending to FCM: %v", err)
			}
			resQueue <- res
		}
		return
	}

	ps.handleMulticastResults(psp, dpList, resQueue, notif, resp.Responses)
}
