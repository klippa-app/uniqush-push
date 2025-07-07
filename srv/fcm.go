package srv

import (
	"fmt"

	"github.com/uniqush/uniqush-push/push"
	"github.com/uniqush/uniqush-push/srv/fcm"
)

// InstallFCM registers the only instance of the FCM push service. It is called only once.
func InstallFCM() {
	psm := push.GetPushServiceManager()
	err := psm.RegisterPushServiceType(fcm.NewPushService())
	if err != nil {
		panic(fmt.Sprintf("Failed to install FCM module: %v", err))
	}
}
