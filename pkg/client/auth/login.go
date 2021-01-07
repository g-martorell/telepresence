package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"golang.org/x/oauth2"

	"github.com/datawire/telepresence2/pkg/client"
)

const (
	callbackPath = "/callback"
)

type oauth2Callback struct {
	Code             string
	Error            string
	ErrorDescription string
}

type LoginExecutor struct {
	Oauth2AuthUrl  string
	Oauth2TokenUrl string
	CompletionUrl  string
	Oauth2ClientId string
	SaveTokenFunc  func(*oauth2.Token) error
	OpenURLFunc    func(string) error
	Scout          *client.Scout
}

func (l *LoginExecutor) LoginFlow(cmd *cobra.Command, args []string) error {
	// oauth2Callback chan that will receive the callback info
	callbacks := make(chan oauth2Callback)
	// also listen for interruption to cancel the flow
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, syscall.SIGINT, syscall.SIGTERM)

	// start the background server on which we'll be listening for the OAuth2 callback
	backgroundServer, err := startBackgroundServer(callbacks, cmd.ErrOrStderr(), l.CompletionUrl)
	defer func() {
		err := backgroundServer.Shutdown(context.Background())
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "error shutting down callback server: %v", err)
		}
	}()
	if err != nil {
		return err
	}
	oauth2Config := oauth2.Config{
		ClientID:    l.Oauth2ClientId,
		RedirectURL: fmt.Sprintf("http://%v%v", backgroundServer.Addr, callbackPath),
		Endpoint: oauth2.Endpoint{
			AuthURL:  l.Oauth2AuthUrl,
			TokenURL: l.Oauth2TokenUrl,
		},
		Scopes: []string{"openid", "profile", "email"},
	}

	// create OAuth2 authentication code flow URL
	state := uuid.New().String()
	pkceVerifier := CreateCodeVerifier()
	url := oauth2Config.AuthCodeURL(
		state,
		oauth2.SetAuthURLParam("code_challenge", pkceVerifier.CodeChallengeS256()),
		oauth2.SetAuthURLParam("code_challenge_method", PKCEChallengeMethodS256),
	)
	fmt.Fprintln(cmd.OutOrStdout(), "Launching browser authentication flow...")
	err = l.OpenURLFunc(url)
	if err != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "Could not open browser, please access this URL: %v\n", url)
	}

	// wait for callback completion or interruption
	select {
	case callback := <-callbacks:
		err := l.handleCallback(cmd, callback, oauth2Config, pkceVerifier)
		if err != nil {
			_ = l.Scout.Report("login_failure", client.ScoutMeta{Key: "error", Value: err.Error()})
		} else {
			// TODO: Should we add ScoutMeta about the logged-in user?
			_ = l.Scout.Report("login_success")
		}
		return err
	case <-interrupts:
		fmt.Fprintln(cmd.OutOrStdout(), "Login aborted.")
		_ = l.Scout.Report("login_interrupted")
		return nil
	}
}

func (l *LoginExecutor) handleCallback(cmd *cobra.Command, callback oauth2Callback, oauth2Config oauth2.Config, pkceVerifier *CodeVerifier) error {
	if callback.Error != "" {
		return fmt.Errorf("%v error returned on OAuth2 callback: %v", callback.Error, callback.ErrorDescription)
	}

	// retrieve access token from callback code
	token, err := oauth2Config.Exchange(
		safeContext(cmd),
		callback.Code,
		oauth2.SetAuthURLParam("code_verifier", pkceVerifier.String()),
	)
	if err != nil {
		return fmt.Errorf("error while exchanging code for token: %w", err)
	}

	err = l.SaveTokenFunc(token)
	if err != nil {
		return fmt.Errorf("could not save access token to user cache: %w", err)
	}

	fmt.Fprintln(cmd.OutOrStdout(), "Login successful.")
	return nil
}

// safeContext is to solve an issue with the tests where the Context in
// cobra.Command can't be set
func safeContext(cmd *cobra.Command) context.Context {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	return ctx
}

func startBackgroundServer(callbacks chan oauth2Callback, stderr io.Writer, completionUrl string) (*http.Server, error) {
	// start listening on the next available port
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return &http.Server{}, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	handler := http.NewServeMux()
	handler.HandleFunc(callbackPath, newCallbackHandlerFunc(callbacks, stderr, completionUrl))
	server := &http.Server{
		Addr:    fmt.Sprintf("localhost:%v", port),
		Handler: handler,
	}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			callbacks <- oauth2Callback{
				Code:             "",
				Error:            "Could not start callback server",
				ErrorDescription: err.Error(),
			}
		}
	}()
	return server, nil
}

func newCallbackHandlerFunc(callbacks chan oauth2Callback, stderr io.Writer, completionUrl string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		code := query.Get("code")
		errorName := query.Get("error")
		errorDescription := query.Get("error_description")

		var sb strings.Builder
		sb.WriteString("<!DOCTYPE html><html><head><title>Authentication Successful</title></head><body>")
		if errorName == "" && code != "" {
			w.Header().Set("Location", completionUrl)
			w.WriteHeader(http.StatusTemporaryRedirect)
			sb.WriteString("<h1>Authentication Successful</h1>")
			sb.WriteString("<p>You can now close this tab and resume on the CLI.</p>")
		} else {
			sb.WriteString("<h1>Authentication Error</h1>")
			sb.WriteString(fmt.Sprintf("<p>%s: %s</p>", errorName, errorDescription))
			w.WriteHeader(http.StatusInternalServerError)
		}
		sb.WriteString("</body></html>")

		w.Header().Set("Content-Type", "text/html")
		_, err := w.Write([]byte(sb.String()))
		if err != nil {
			fmt.Fprintf(stderr, "Error writing callback response body: %v", err)
		}

		callbacks <- oauth2Callback{
			Code:             code,
			Error:            errorName,
			ErrorDescription: errorDescription,
		}
	}
}
