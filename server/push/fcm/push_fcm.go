// Package fcm implements push notification plugin for Google FCM backend.
// Push notifications for Android, iOS and web clients are sent through Google's Firebase Cloud Messaging service.
// Package fcm is push notification plugin using Google FCM.
// https://firebase.google.com/docs/cloud-messaging
package fcm

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	fbase "firebase.google.com/go/v4"
	fbmsg "firebase.google.com/go/v4/messaging"

	"github.com/tinode/chat/server/logs"
	"github.com/tinode/chat/server/push"
	"github.com/tinode/chat/server/push/common"
	"github.com/tinode/chat/server/store"
	"github.com/tinode/chat/server/store/types"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
)

var handler Handler

const (
	// Size of the input channel buffer.
	bufferSize = 1024

	// The number of push messages sent in one batch. FCM constant.
	pushBatchSize = 100

	// The number of sub/unsub requests sent in one batch. FCM constant.
	subBatchSize = 1000
)

// Handler represents the push handler; implements push.PushHandler interface.
type Handler struct {
	input   chan *push.Receipt
	channel chan *push.ChannelReq
	stop    chan bool

	ctx    context.Context
	client *fbmsg.Client
}

type configType struct {
	Enabled         bool            `json:"enabled"`
	DryRun          bool            `json:"dry_run"`
	Credentials     json.RawMessage `json:"credentials"`
	CredentialsFile string          `json:"credentials_file"`
	TimeToLive      int             `json:"time_to_live,omitempty"`
	ApnsBundleID    string          `json:"apns_bundle_id,omitempty"`
	Android         *common.Config  `json:"android,omitempty"`
	Apns            *common.Config  `json:"apns,omitempty"`
	Webpush         *common.Config  `json:"webpush,omitempty"`
}

// Init initializes the push handler
func (Handler) Init(jsonconf json.RawMessage) (bool, error) {

	var config configType
	err := json.Unmarshal([]byte(jsonconf), &config)
	if err != nil {
		return false, errors.New("failed to parse config: " + err.Error())
	}

	if !config.Enabled {
		return false, nil
	}

	if config.Credentials == nil && config.CredentialsFile != "" {
		config.Credentials, err = os.ReadFile(config.CredentialsFile)
		if err != nil {
			return false, err
		}
	}

	if config.Credentials == nil {
		return false, errors.New("missing credentials")
	}

	handler.ctx = context.Background()
	credentials, err := google.CredentialsFromJSON(handler.ctx, config.Credentials, "https://www.googleapis.com/auth/firebase.messaging")
	if err != nil {
		return false, err
	}

	app, err := fbase.NewApp(handler.ctx, &fbase.Config{}, option.WithCredentials(credentials))
	if err != nil {
		return false, err
	}

	handler.client, err = app.Messaging(handler.ctx)
	if err != nil {
		return false, err
	}

	handler.input = make(chan *push.Receipt, bufferSize)
	handler.channel = make(chan *push.ChannelReq, bufferSize)
	handler.stop = make(chan bool, 1)

	go func() {
		for {
			select {
			case rcpt := <-handler.input:
				go sendFcm(rcpt, &config)
			case sub := <-handler.channel:
				go processSubscription(sub)
			case <-handler.stop:
				return
			}
		}
	}()

	return true, nil
}

func sendFcm(rcpt *push.Receipt, config *configType) {
	messages, uids := PrepareNotifications(rcpt, config)
	for i := range messages {
		var err error
		if config.DryRun {
			_, err = handler.client.SendDryRun(handler.ctx, messages[i])
		} else {
			_, err = handler.client.Send(handler.ctx, messages[i])
		}

		if err != nil {
			if fbmsg.IsQuotaExceeded(err) || fbmsg.IsUnavailable(err) || fbmsg.IsInternal(err) {
				// Transient errors. Stop sending this batch.
				logs.Warn.Println("fcm transient failure:", err.Error())
				return
			}
			if fbmsg.IsSenderIDMismatch(err) || fbmsg.IsInvalidArgument(err) || fbmsg.IsThirdPartyAuthError(err) {
				// Config errors. Stop.
				logs.Warn.Println("fcm invalid config:", err.Error())
				return
			}

			if fbmsg.IsUnregistered(err) {
				// Token is no longer valid. Delete token from DB and continue sending.
				logs.Warn.Println("fcm invalid token:", err.Error())
				if err := store.Devices.Delete(uids[i], messages[i].Token); err != nil {
					logs.Warn.Println("fcm failed to delete invalid token:", err)
				}
			} else {
				// Unknown error. Stop sending just in case.
				logs.Warn.Println("fcm unrecognized error:", err.Error())
				return
			}
		}
	}
}

func processSubscription(req *push.ChannelReq) {
	var channel string
	var devices []string
	var device string
	var channels []string

	if req.Channel != "" {
		devices = DevicesForUser(req.Uid)
		channel = req.Channel
	} else if req.DeviceID != "" {
		channels = ChannelsForUser(req.Uid)
		device = req.DeviceID
	}

	if (len(devices) == 0 && device == "") || (len(channels) == 0 && channel == "") {
		// No channels or devces to subscribe or unsubscribe.
		return
	}

	if len(devices) > subBatchSize {
		// It's extremely unlikely for a single user to have this many devices.
		devices = devices[0:subBatchSize]
		logs.Warn.Println("fcm: user", req.Uid.UserId(), "has more than", subBatchSize, "devices")
	}

	var err error
	var resp *fbmsg.TopicManagementResponse
	if channel != "" && len(devices) > 0 {
		if req.Unsub {
			resp, err = handler.client.UnsubscribeFromTopic(context.Background(), devices, channel)
		} else {
			resp, err = handler.client.SubscribeToTopic(context.Background(), devices, channel)
		}
		if err != nil {
			// Complete failure.
			logs.Warn.Println("fcm: sub or upsub failed", req.Unsub, err)
		} else {
			// Check for partial failure.
			handleSubErrors(resp, req.Uid, devices)
		}
		return
	}

	if device != "" && len(channels) > 0 {
		devices := []string{device}
		for _, channel := range channels {
			if req.Unsub {
				resp, err = handler.client.UnsubscribeFromTopic(context.Background(), devices, channel)
			} else {
				resp, err = handler.client.SubscribeToTopic(context.Background(), devices, channel)
			}
			if err != nil {
				// Complete failure.
				logs.Warn.Println("fcm: sub or upsub failed", req.Unsub, err)
				break
			}
			// Check for partial failure.
			handleSubErrors(resp, req.Uid, devices)
		}
		return
	}

	// Invalid request: either multiple channels & multiple devices (not supported) or no channels and no devices.
	logs.Err.Println("fcm: user", req.Uid.UserId(), "invalid combination of sub/unsub channels/devices",
		len(devices), len(channels))
}

func handleSubErrors(response *fbmsg.TopicManagementResponse, uid types.Uid, devices []string) {
	if response.FailureCount <= 0 {
		return
	}

	for _, errinfo := range response.Errors {
		// FCM documentation sucks. There is no list of possible errors so no action can be taken but logging.
		logs.Warn.Println("fcm sub/unsub error", errinfo.Reason, uid, devices[errinfo.Index])
	}
}

// IsReady checks if the push handler has been initialized.
func (Handler) IsReady() bool {
	return handler.input != nil
}

// Push returns a channel that the server will use to send messages to.
// If the adapter blocks, the message will be dropped.
func (Handler) Push() chan<- *push.Receipt {
	return handler.input
}

// Channel returns a channel for subscribing/unsubscribing devices to FCM topics.
func (Handler) Channel() chan<- *push.ChannelReq {
	return handler.channel
}

// Stop shuts down the handler
func (Handler) Stop() {
	handler.stop <- true
}

func init() {
	push.Register("fcm", &handler)
}
