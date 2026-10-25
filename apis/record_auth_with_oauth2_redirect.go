package apis

import (
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/tools/subscriptions"
	"github.com/Robernetes/popgres/ui"
)

const (
	oauth2SubscriptionTopic               string = "@oauth2"
	oauth2RedirectFailurePath             string = "../_/#/auth/oauth2-redirect-failure"
	oauth2RedirectSuccessPath             string = "../_/#/auth/oauth2-redirect-success"
	oauth2RedirectAppleNameStoreKeyPrefix string = "@redirect_name_"
)

type oauth2RedirectData struct {
	State string `form:"state" json:"state"`
	Code  string `form:"code" json:"code"`
	Error string `form:"error" json:"error,omitempty"`

	// returned by Apple only
	AppleUser string `form:"user" json:"-"`
}

func oauth2SubscriptionRedirect(e *core.RequestEvent) error {
	data := oauth2RedirectData{}

	if e.Request.Method == http.MethodPost {
		if err := e.BindBody(&data); err != nil {
			e.App.Logger().Debug("Failed to read OAuth2 redirect data", "error", err)
			return failureRedirect(e)
		}
	} else {
		query := e.Request.URL.Query()
		data.State = query.Get("state")
		data.Code = query.Get("code")
		data.Error = query.Get("error")
	}

	if data.State == "" {
		e.App.Logger().Debug("Missing OAuth2 state parameter")
		return failureRedirect(e)
	}

	client, err := e.App.SubscriptionsBroker().ClientById(data.State)

	// popgres: the client could be connected to another app instance (node)
	if err != nil {
		if nodeId, ok := shouldForwardToClientNode(e, data.State); ok {
			form := url.Values{}
			form.Set("state", data.State)
			form.Set("code", data.Code)
			if data.Error != "" {
				form.Set("error", data.Error)
			}
			if data.AppleUser != "" {
				form.Set("user", data.AppleUser)
			}

			// note: forwarded always as POST since the data is already parsed
			e.Request.Method = http.MethodPost

			forwardErr := forwardRequest(e, nodeId, []byte(form.Encode()), "application/x-www-form-urlencoded")
			if forwardErr != nil {
				e.App.Logger().Debug("Failed to forward the OAuth2 redirect", "error", forwardErr, "clientId", data.State)
				return failureRedirect(e)
			}

			return nil
		}
	}

	if err != nil || client.IsDiscarded() || !client.HasSubscription(oauth2SubscriptionTopic) {
		e.App.Logger().Debug("Missing or invalid OAuth2 subscription client", "error", err, "clientId", data.State)
		return failureRedirect(e)
	}
	defer client.Unsubscribe(oauth2SubscriptionTopic)

	// additional check to minimize the risk of XSRF attack vectors
	//
	// note: custom registered clients (aka. those without IP in the store)
	// are excluded from the check for backward compatibility
	clientIP, _ := client.Get(RealtimeClientIPKey).(string)
	if clientIP != "" && clientIP != e.RealIP() {
		e.App.Logger().Debug("The client IP that completed the authentication is different from the one that initialized the OAuth2 realtime connection")
		return failureRedirect(e)
	}

	// temporary store the Apple user's name so that it can be later retrieved with the authWithOAuth2 call
	// (see https://github.com/pocketbase/pocketbase/issues/7090)
	if data.AppleUser != "" && data.Error == "" && data.Code != "" {
		nameErr := parseAndStoreAppleRedirectName(
			e.App,
			oauth2RedirectAppleNameStoreKeyPrefix+data.Code,
			data.AppleUser,
		)
		if nameErr != nil {
			// non-critical error
			e.App.Logger().Debug("Failed to parse and load Apple Redirect name data", "error", nameErr)
		}
	}

	encodedData, err := json.Marshal(data)
	if err != nil {
		e.App.Logger().Debug("Failed to marshalize OAuth2 redirect data", "error", err)
		return failureRedirect(e)
	}

	msg := subscriptions.Message{
		Name: oauth2SubscriptionTopic,
		Data: encodedData,
	}

	client.Send(msg)

	if data.Error != "" || data.Code == "" {
		e.App.Logger().Debug("Failed OAuth2 redirect due to an error or missing code parameter", "error", data.Error, "clientId", data.State)
		return failureRedirect(e)
	}

	return successRedirect(e)
}

func redirectStatusCode(e *core.RequestEvent) int {
	if e.Request.Method != http.MethodGet {
		return http.StatusSeeOther
	}

	return http.StatusTemporaryRedirect
}

func failureRedirect(e *core.RequestEvent) error {
	// fallback if UI is not bundled
	if ui.DistDirFS == nil {
		return e.String(http.StatusOK, "Failed to authenticate. You can close this window and go back to the app to try again.")
	}

	return e.Redirect(redirectStatusCode(e), oauth2RedirectFailurePath)
}

func successRedirect(e *core.RequestEvent) error {
	// fallback if UI is not bundled
	if ui.DistDirFS == nil {
		return e.HTML(http.StatusOK, "Auth completed. You can close this window and go back to the app.")
	}

	return e.Redirect(redirectStatusCode(e), oauth2RedirectSuccessPath)
}

// parseAndStoreAppleRedirectName extracts the first and last name
// from serializedNameData and temporary store them in the app.Store.
//
// This is hacky workaround to forward safely and seamlessly the Apple
// redirect user's name back to the OAuth2 auth handler.
//
// Note that currently Apple is the only provider that behaves like this and
// for now it is unnecessary to check whether the redirect is coming from Apple or not.
//
// Ideally this shouldn't be needed and will be removed in the future
// once Apple adds a dedicated userinfo endpoint.
func parseAndStoreAppleRedirectName(app core.App, nameKey string, serializedNameData string) error {
	if serializedNameData == "" {
		return nil
	}

	// just in case to prevent storing large strings in memory
	if len(nameKey) > 1000 {
		return errors.New("nameKey is too large")
	}

	// https://developer.apple.com/documentation/signinwithapple/incorporating-sign-in-with-apple-into-other-platforms#Handle-the-response
	extracted := struct {
		Name struct {
			FirstName string `json:"firstName"`
			LastName  string `json:"lastName"`
		} `json:"name"`
	}{}
	if err := json.Unmarshal([]byte(serializedNameData), &extracted); err != nil {
		return err
	}

	fullName := extracted.Name.FirstName + " " + extracted.Name.LastName

	// truncate just in case to prevent storing large strings in memory
	if len(fullName) > 150 {
		fullName = fullName[:150]
	}

	fullName = strings.TrimSpace(fullName)
	if fullName == "" {
		return nil
	}

	// store (with expiration) in the store shared by all app instances (nodes)
	return app.Cluster().KV().Set(nameKey, fullName, 1*time.Minute)
}
