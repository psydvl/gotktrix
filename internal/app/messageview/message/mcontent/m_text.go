package mcontent

import (
	"context"
	"encoding/json"

	"github.com/chanbakjsd/gotrix/event"
	"github.com/chanbakjsd/gotrix/matrix"
	"github.com/diamondburned/gotk4/pkg/core/glib"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotktrix/internal/app"
	"github.com/diamondburned/gotktrix/internal/app/messageview/message/mcontent/text"
	"github.com/diamondburned/gotktrix/internal/gotktrix"
	"github.com/diamondburned/gotktrix/internal/gtkutil/cssutil"
	"github.com/diamondburned/gotktrix/internal/locale"
	"github.com/diamondburned/gotktrix/internal/md"
)

type textContent struct {
	*gtk.Box
	text   *gtk.TextView
	embeds *gtk.Box

	ctx context.Context
}

var _ editableContentPart = (*textContent)(nil)

var textContentCSS = cssutil.Applier("mcontent-text", `
	textview.mcontent-text,
	textview.mcontent-text text {
		background-color: transparent;
	}
`)

func newTextContent(ctx context.Context, msgBox *gotktrix.EventBox) *textContent {
	tview := gtk.NewTextView()
	tview.SetEditable(false)
	tview.SetAcceptsTab(false)
	tview.SetCursorVisible(false)
	tview.SetWrapMode(gtk.WrapWordChar)

	md.SetTabSize(tview)
	textContentCSS(tview)

	text.BindLinkHandler(tview, func(url string) {
		app.OpenURI(ctx, url)
	})

	c := textContent{
		Box:  gtk.NewBox(gtk.OrientationVertical, 0),
		text: tview,
		ctx:  ctx,
	}

	c.Box.Append(tview)

	body, isEdited := msgBody(msgBox)
	c.setContent(body, isEdited)

	tview.ConnectMap(func() {
		c.invalidateAllocate()
	})

	return &c
}

func (c *textContent) content() {}

func (c *textContent) SetExtraMenu(menu gio.MenuModeller) {
	gmenu := gio.NewMenu()
	gmenu.InsertSection(0, "Message", menu)
	c.text.SetExtraMenu(gmenu)
}

func (c *textContent) edit(body messageBody) {
	c.setContent(body, true)
}

func (c *textContent) setContent(body messageBody, isEdited bool) {
	buf := c.text.Buffer()
	buf.SetText("")

	var meta text.RenderMetadata

	switch body.Format {
	case event.FormatHTML:
		meta = text.RenderHTML(c.ctx, c.text, body.Body, body.FormattedBody)
	default:
		meta = text.RenderText(c.ctx, c.text, body.Body)
	}

	if c.embeds != nil {
		c.Box.Remove(c.embeds)
	}

	// We need to wrap the message inside a box if we need embeds.
	if len(meta.URLs) > 0 {
		c.embeds = gtk.NewBox(gtk.OrientationVertical, 0)
		c.embeds.AddCSSClass("mcontent-embeds")
		c.Box.Append(c.embeds)
		// TODO: cancellation
		loadEmbeds(c.ctx, c.embeds, meta.URLs)
	}

	if isEdited {
		end := buf.EndIter()

		edited := `<span alpha="75%" size="small">` + locale.S(c.ctx, "(edited)") + "</span>"
		if buf.CharCount() > 0 {
			// Prepend a space if we already have text.
			edited = " " + edited
		}

		buf.InsertMarkup(end, edited)
	}

	c.invalidateAllocate()
}

func (c *textContent) invalidateAllocate() {
	// Workaround to hopefully fix 2 GTK bugs:
	// - TextViews are invisible sometimes.
	// - Multiline TextViews are sometimes only drawn as 1.
	glib.TimeoutAdd(100, func() {
		c.text.QueueAllocate()
		c.text.QueueResize()
	})
}

type messageBody struct {
	Body          string              `json:"body"`
	MsgType       event.MessageType   `json:"msgtype"`
	Format        event.MessageFormat `json:"format,omitempty"`
	FormattedBody string              `json:"formatted_body,omitempty"`
}

func msgBody(box *gotktrix.EventBox) (m messageBody, edited bool) {
	var body struct {
		messageBody
		NewContent messageBody `json:"m.new_content"`

		RelatesTo struct {
			RelType string         `json:"rel_type"`
			EventID matrix.EventID `json:"event_id"`
		} `json:"m.relates_to"`
	}

	if err := json.Unmarshal(box.Content, &body); err != nil {
		// This shouldn't happen, since we already unmarshaled above.
		return messageBody{}, false
	}

	if body.RelatesTo.RelType == "m.replace" {
		return body.NewContent, true
	}
	return body.messageBody, false
}
