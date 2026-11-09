package wizard

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"

	"github.com/jezmor/monarch-to-sure/web"
)

const maxBodyBytes = 200 << 20

// Handler serves the page and the wizard's API.
func Handler(w *Wizard, token string, port func() int) http.Handler {
	routes := map[string]func(body map[string]json.RawMessage) (any, error){
		"/api/connect": func(b map[string]json.RawMessage) (any, error) {
			return w.Connect(str(b["url"]), str(b["api_key"]))
		},
		"/api/files": func(b map[string]json.RawMessage) (any, error) {
			var transactions *string
			_ = json.Unmarshal(b["transactions"], &transactions)
			return w.LoadFiles(str(b["balances"]), transactions)
		},
		"/api/preview": func(b map[string]json.RawMessage) (any, error) {
			choices := map[string]Choice{}
			if raw, ok := b["choices"]; ok {
				if err := json.Unmarshal(raw, &choices); err != nil {
					return nil, userErr("the account choices couldn't be read: %v", err)
				}
			}
			return w.Preview(choices, boolean(b["include_pending"]))
		},
		"/api/import": func(b map[string]json.RawMessage) (any, error) {
			return w.StartImport(boolean(b["allow_existing_accounts"]))
		},
		"/api/status": func(map[string]json.RawMessage) (any, error) { return w.Status(), nil },
	}

	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Cache-Control", "no-store")
		rw.Header().Set("X-Frame-Options", "DENY")
		// Rejects DNS-rebinding requests that reach this port under another name.
		if p := port(); r.Host != fmt.Sprintf("127.0.0.1:%d", p) && r.Host != fmt.Sprintf("localhost:%d", p) {
			writeJSON(rw, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		if r.Method == http.MethodGet {
			if r.URL.Path != "/" {
				writeJSON(rw, http.StatusNotFound, map[string]string{"error": "not found"})
				return
			}
			rw.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = rw.Write(web.WizardHTML)
			return
		}
		if r.Method != http.MethodPost {
			writeJSON(rw, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		// The token comes from the link the wizard opened, and is sent as a
		// header so that other web pages can't drive the wizard.
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Wizard-Token")), []byte(token)) != 1 {
			writeJSON(rw, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		route, ok := routes[r.URL.Path]
		if !ok {
			writeJSON(rw, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, maxBodyBytes))
		if err != nil {
			writeJSON(rw, http.StatusRequestEntityTooLarge, map[string]string{"error": "the files are too large"})
			return
		}
		body := map[string]json.RawMessage{}
		if len(data) > 0 {
			if err := json.Unmarshal(data, &body); err != nil {
				writeJSON(rw, http.StatusBadRequest, map[string]string{"error": "the request wasn't valid JSON"})
				return
			}
		}
		result, err := route(body)
		if err != nil {
			writeJSON(rw, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(rw, http.StatusOK, result)
	})
}

func str(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func boolean(raw json.RawMessage) bool {
	var b bool
	_ = json.Unmarshal(raw, &b)
	return b
}

func writeJSON(rw http.ResponseWriter, status int, value any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(value)
}

// NewToken returns a random token for the wizard link.
func NewToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Run serves the wizard until interrupted. With baseURL and apiKey it
// connects first, so the page can skip that step.
func Run(port int, openBrowser bool, baseURL, apiKey string) error {
	w := New()
	token := NewToken()
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		if listener, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return err
		}
	}
	actualPort := listener.Addr().(*net.TCPAddr).Port

	link := fmt.Sprintf("http://127.0.0.1:%d/#%s", actualPort, token)
	if baseURL != "" {
		link += "&url=" + url.QueryEscape(baseURL)
	}
	if baseURL != "" && apiKey != "" {
		if _, err := w.Connect(baseURL, apiKey); err != nil {
			fmt.Printf("Couldn't connect with SURE_URL and SURE_API_KEY: %v\n", err)
		} else {
			link += "&connected=1"
		}
	}

	fmt.Printf("Monarch to Sure wizard: %s\n", link)
	fmt.Println("Keep this window open while you use it. Press Ctrl+C to stop.")
	if openBrowser {
		open(link)
	}

	server := &http.Server{Handler: Handler(w, token, func() int { return actualPort })}
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, os.Interrupt)
	go func() {
		<-interrupted
		fmt.Println()
		_ = server.Close()
	}()
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func open(link string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", link)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", link)
	default:
		cmd = exec.Command("xdg-open", link)
	}
	if err := cmd.Start(); err != nil {
		fmt.Printf("Open the link above in your browser (%v).\n", err)
	}
}
