// Package auth supplies a gtk.Assistant wrapper to provide a login screen.
package auth

import (
	"log"

	"github.com/chanbakjsd/gotrix/api/httputil"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
	"github.com/diamondburned/gotktrix/internal/auth/secret"
	"github.com/diamondburned/gotktrix/internal/components/assistant"
	"github.com/diamondburned/gotktrix/internal/config"
	"github.com/diamondburned/gotktrix/internal/gotktrix"
	"github.com/diamondburned/gotktrix/internal/gtkutil/cssutil"
	"github.com/diamondburned/gotktrix/internal/gtkutil/markuputil"
)

var (
	keyringAppID   = config.AppIDDot("secrets")
	encryptionPath = config.Path("secrets")
)

type Assistant struct {
	*assistant.Assistant
	client httputil.Client

	onConnect func(*gotktrix.Client, *Account)

	// states, can be nil depending on the steps
	accounts      []*Account
	currentClient *gotktrix.Client

	keyring *secret.Keyring
	encrypt *secret.EncryptedFile

	// hasConnected is true if the connection has already been connected.
	hasConnected bool
}

type discoverStep struct {
	// states
	serverName string
}

// New creates a new authentication assistant with the default HTTP client.
func New(parent *gtk.Window) *Assistant {
	return NewWithClient(parent, httputil.NewClient())
}

// NewWithClient creates a new authentication assistant with the given HTTP
// client.
func NewWithClient(parent *gtk.Window, client httputil.Client) *Assistant {
	ass := assistant.New(parent, nil)
	ass.SetTitle("Getting Started")

	a := Assistant{
		Assistant: ass,
		client:    client,
		keyring:   secret.KeyringDriver(keyringAppID),
	}

	ass.Connect("close-request", func() {
		// If the user hasn't chosen to connect to anything yet, then exit the
		// main window as well.
		if !a.hasConnected {
			parent.Close()
		}
	})
	ass.AddStep(accountChooserStep(&a))
	return &a
}

// OnConnect sets the handler that is called when the user chooses an account or
// logs in. If this method has already been called before with a non-nil
// function, it will panic.
func (a *Assistant) OnConnect(f func(*gotktrix.Client, *Account)) {
	if a.onConnect != nil {
		panic("OnConnect called twice")
	}

	a.onConnect = f
}

// step 1 activate
func (a *Assistant) signinPage() {
	step2 := homeserverStep(a)
	a.AddStep(step2)
	a.SetStep(step2)

	step3 := chooseLoginStep(a)
	a.AddStep(step3)
}

// step 2 activate
func (a *Assistant) chooseHomeserver(client *gotktrix.Client) {
	a.currentClient = client
	a.NextStep() // step 2 -> 3
}

// step 3 activate
func (a *Assistant) chooseLoginMethod(method loginMethod) {
	step4 := loginStep(a, method)
	a.AddStep(step4)
	a.SetStep(step4)
}

// finish should be called once a.currentClient has been logged on.
func (a *Assistant) finish(acc *Account) {
	if a.onConnect == nil {
		log.Println("onConnect handler not attached")
		return
	}

	a.hasConnected = true
	a.Continue()
	a.Close()
	a.onConnect(a.currentClient, acc)
}

var inputBoxCSS = cssutil.Applier("auth-input-box", `
	.auth-input-box {
		margin-top: 4px;
	}
	.auth-input-box label {
		margin-left: .5em;
	}
	.auth-input-box > entry {
		margin-bottom: 4px;
	}
`)

var inputLabelAttrs = markuputil.Attrs(
	pango.NewAttrForegroundAlpha(65535 * 90 / 100), // 90%
)

func (a *Assistant) makeInputs(names ...string) (gtk.Widgetter, []*gtk.Entry) {
	box := gtk.NewBox(gtk.OrientationVertical, 0)
	box.SetSizeRequest(200, -1)
	inputBoxCSS(box)

	entries := make([]*gtk.Entry, len(names))

	for i, name := range names {
		label := gtk.NewLabel(name)
		label.SetXAlign(0)
		label.SetAttributes(inputLabelAttrs)

		entry := gtk.NewEntry()
		entry.SetEnableUndo(true)

		if i < len(names)-1 {
			// Enter moves to the next entry.
			next := i + 1
			entry.Connect("activate", func() { entries[next].GrabFocus() })
		} else {
			// Enter hits the OK button.
			entry.Connect("activate", func() { a.OKButton().Activate() })
		}

		box.Append(label)
		box.Append(entry)

		entries[i] = entry
	}

	return box, entries
}

var errorLabelCSS = cssutil.Applier("auth-error-label", `
	.auth-error-label {
		padding-top: 4px;
	}
`)

func makeErrorLabel() *gtk.Label {
	errLabel := markuputil.ErrorLabel("")
	errorLabelCSS(errLabel)
	return errLabel
}
