package gotktrix

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chanbakjsd/gotrix"
	"github.com/chanbakjsd/gotrix/api"
	"github.com/chanbakjsd/gotrix/api/httputil"
	"github.com/chanbakjsd/gotrix/event"
	"github.com/chanbakjsd/gotrix/matrix"
	"github.com/diamondburned/gotktrix/internal/config"
	"github.com/diamondburned/gotktrix/internal/gotktrix/events/m"
	"github.com/diamondburned/gotktrix/internal/gotktrix/events/sys"
	"github.com/diamondburned/gotktrix/internal/gotktrix/indexer"
	"github.com/diamondburned/gotktrix/internal/gotktrix/internal/db"
	"github.com/diamondburned/gotktrix/internal/gotktrix/internal/handler"
	"github.com/diamondburned/gotktrix/internal/gotktrix/internal/httptrick"
	"github.com/diamondburned/gotktrix/internal/gotktrix/internal/state"
	"github.com/pkg/errors"
)

// EachBreak can be returned if the user wants to break out of an interation.
var EachBreak = db.EachBreak

// TimelimeLimit is the number of timeline events that the database keeps.
const TimelimeLimit = state.TimelineKeepLast

var SyncOptions = gotrix.SyncOptions{
	Filter: event.GlobalFilter{
		Room: event.RoomFilter{
			IncludeLeave: true,
			State: event.StateFilter{
				LazyLoadMembers: true,
			},
			Timeline: event.RoomEventFilter{
				Limit:           TimelimeLimit,
				LazyLoadMembers: true,
			},
		},
	},
	Timeout:        time.Minute,
	MinBackoffTime: 2 * time.Second,
	MaxBackoffTime: 10 * time.Second,
}

var deviceName = "gotktrix"

func init() {
	hostname, err := os.Hostname()
	if err == nil {
		deviceName += " (" + hostname + ")"
	}
}

var (
	cancelled  context.Context
	cancelOnce sync.Once
)

// Cancelled gets a cancelled context.
func Cancelled() context.Context {
	cancelOnce.Do(func() {
		var cancel func()

		cancelled, cancel = context.WithCancel(context.Background())
		cancel()
	})

	return cancelled
}

type ctxKey uint

const (
	clientCtxKey ctxKey = iota
)

// WithClient injects the given client into a new context.
func WithClient(ctx context.Context, c *Client) context.Context {
	return context.WithValue(ctx, clientCtxKey, c)
}

// FromContext returns the client inside the context wrapped with WithClient. If
// the context isn't yet wrapped, then nil is returned.
func FromContext(ctx context.Context) *Client {
	c, _ := ctx.Value(clientCtxKey).(*Client)
	if c != nil {
		return c.WithContext(ctx)
	}
	return nil
}

// ClientAuth holds a partial client.
type ClientAuth struct {
	c *gotrix.Client
}

// Discover wraps around gotrix.DiscoverWithClienT.
func Discover(hcl httputil.Client, serverName string) (*ClientAuth, error) {
	c, err := gotrix.DiscoverWithClient(hcl, serverName)
	if err != nil {
		return nil, err
	}

	return &ClientAuth{c}, nil
}

// WithContext creates a copy of ClientAuth that uses the provided context.
func (a *ClientAuth) WithContext(ctx context.Context) *ClientAuth {
	return &ClientAuth{c: a.c.WithContext(ctx)}
}

// LoginPassword authenticates the client using the provided username and
// password.
func (a *ClientAuth) LoginPassword(username, password string) (*Client, error) {
	err := a.c.Client.Login(api.LoginArg{
		Type: matrix.LoginPassword,
		Identifier: matrix.Identifier{
			Type: matrix.IdentifierUser,
			User: username,
		},
		Password:                 password,
		InitialDeviceDisplayName: deviceName,
	})
	if err != nil {
		return nil, err
	}
	return wrapClient(a.c)
}

// LoginToken authenticates the client using the provided token.
func (a *ClientAuth) LoginToken(token string) (*Client, error) {
	err := a.c.Client.Login(api.LoginArg{
		Type:                     matrix.LoginToken,
		Token:                    deviceName,
		InitialDeviceDisplayName: deviceName,
	})
	if err != nil {
		return nil, err
	}
	return wrapClient(a.c)
}

// LoginSSO returns the HTTP address for logging in as SSO and the channel
// indicating if the user is done or not.
func (a *ClientAuth) LoginSSO(done func(*Client, error)) (string, error) {
	address, wait, err := a.c.LoginSSO()
	if err != nil {
		return "", err
	}

	go func() {
		if err := wait(); err != nil {
			done(nil, err)
			return
		}

		done(wrapClient(a.c))
	}()

	return address, nil
}

// LoginMethods returns the login methods supported by the homeserver.
func (a *ClientAuth) LoginMethods() ([]matrix.LoginMethod, error) {
	return a.c.Client.GetLoginMethods()
}

type Client struct {
	*gotrix.Client
	*handler.Registry
	State       *state.State
	Index       *indexer.Indexer
	Interceptor *httptrick.Interceptor

	ctx context.Context
}

// New wraps around gotrix.NewWithClient.
func New(hcl httputil.Client, serverName string, uID matrix.UserID, token string) (*Client, error) {
	c, err := gotrix.NewWithClient(hcl, serverName)
	if err != nil {
		return nil, err
	}

	c.UserID = uID
	c.AccessToken = token

	return wrapClient(c)
}

/*
var cachedRoutes = map[string]map[string]string{
	// TODO: this doesn't work. Investigate.
	"/_matrix/media/r0/*": {
		"Cache-Control": httptrick.OverrideCacheControl(4 * time.Hour),
	},
}
*/

func wrapClient(c *gotrix.Client) (*Client, error) {
	logInit()

	if c.UserID == "" {
		userID, _, err := c.Whoami()
		if err != nil {
			return nil, errors.Wrap(err, "invalid user account")
		}
		c.UserID = userID
	}

	// URLEncoding is path-safe; StdEncoding is not.
	b64Username := base64.URLEncoding.EncodeToString([]byte(c.UserID))

	interceptor := httptrick.WrapInterceptor(http.DefaultTransport)
	c.ClientDriver = &http.Client{
		Transport: interceptor,
	}

	s, err := state.New(config.Path("matrix-state", b64Username), c.UserID)
	if err != nil {
		return nil, errors.Wrap(err, "failed to make state db")
	}

	idx, err := indexer.Open(config.Path("matrix-index", b64Username))
	if err != nil {
		return nil, errors.Wrap(err, "failed to make indexer")
	}

	registry := handler.New()
	registry.OnSync(func(s *api.SyncResponse) {
		for _, room := range s.Rooms.Joined {
			for _, ev := range room.State.Events {
				if state.GuessType(ev) != event.TypeRoomMember {
					continue
				}

				if member, ok := sys.Parse(ev).(*event.RoomMemberEvent); ok {
					b := idx.Begin()
					b.IndexRoomMember(member)
					b.Commit()
				}
			}
		}
	})

	c.State = registry.Wrap(s)
	c.SyncOpts = SyncOptions

	return &Client{
		Client:      c,
		Registry:    registry,
		State:       s,
		Index:       idx,
		Interceptor: interceptor,
	}, nil
}

// AddHandler will panic.
//
// Deprecated: Use c.On() instead.
func (c *Client) AddHandler(function interface{}) error {
	panic("don't use AddHandler(); use On().")
}

// Open opens the client with the last next batch string.
func (c *Client) Open() error {
	next, _ := c.State.NextBatch()
	return c.Client.OpenWithNext(next)
}

// Close closes the event loop and the internal database, as well as halting all
// ongoing requests.
func (c *Client) Close() error {
	err1 := c.Client.Close()
	err2 := c.State.Close()

	if err1 != nil {
		return err1
	}
	return err2
}

// Offline returns a Client that does not use the API.
func (c *Client) Offline() *Client {
	return c.WithContext(Cancelled())
}

// Online returns a Client that uses the given context instead of the cancelled
// context. It is an alias to WithContext; the only difference is that the name
// implies the client may be offline prior to this call.
func (c *Client) Online(ctx context.Context) *Client {
	return c.WithContext(ctx)
}

// AddSyncInterceptFull adds an InterceptFullFunc for the Sync endpoint.
func (c *Client) AddSyncInterceptFull(f httptrick.InterceptFullFunc) func() {
	return c.Interceptor.AddInterceptFull(
		func(r *http.Request, next func() (*http.Response, error)) (*http.Response, error) {
			// Beware: api.EndpointX doesn't have a prefixing slash!
			if strings.HasPrefix(r.URL.Path, "/"+api.EndpointSync) {
				return f(r, next)
			}
			return next()
		},
	)
}

// WithContext replaces the client's internal context with the given one.
func (c *Client) WithContext(ctx context.Context) *Client {
	cpy := *c
	cpy.ctx = ctx
	cpy.Client = cpy.Client.WithContext(ctx)
	return &cpy
}

// Whoami is a cached version of the Whoami method.
func (c *Client) Whoami() (matrix.UserID, error) {
	return c.UserID, nil
}

// SquareThumbnail is a helper function around MediaThumbnailURL. The given size
// is assumed to be a square, and the size will be scaled up to the next power
// of 2 and multiplied up for ensured HiDPI support of up to 2x.
func (c *Client) SquareThumbnail(mURL matrix.URL, size, scale int) (string, error) {
	return c.Thumbnail(mURL, size, size, scale)
}

var errEmptyURL = errors.New("empty Matrix URL")

// Thumbnail is a helper function around MediaThumbnailURL. It works similarly
// to SquareThumbnail, except the dimensions are unchanged.
func (c *Client) Thumbnail(mURL matrix.URL, w, h, scale int) (string, error) {
	if mURL == "" || w == 0 || h == 0 || scale == 0 {
		return "", errEmptyURL
	}

	w *= scale
	h *= scale

	s, err := c.MediaThumbnailURL(mURL, true, w, h, api.MediaThumbnailCrop)
	if err != nil {
		return s, err
	}

	return makeScaledURL(s, scale), nil
}

func makeScaledURL(s string, scale int) string {
	u, err := url.Parse(s)
	if err != nil {
		return s
	}

	// Make the scaling part of the URL too.
	if u.Fragment == "" {
		u.Fragment = fmt.Sprintf("scale=%d", scale)
	} else {
		u.Fragment += fmt.Sprintf("&scale=%d", scale)
	}

	return u.String()
}

// ScaledThumbnail is like Thumbnail, except the image URL in the image
// respects the original aspect ratio and not the requested one.
func (c *Client) ScaledThumbnail(mURL matrix.URL, w, h, scale int) (string, error) {
	if mURL == "" {
		return "", errEmptyURL
	}

	w *= scale
	h *= scale

	s, err := c.MediaThumbnailURL(mURL, true, w, h, api.MediaThumbnailScale)
	if err != nil {
		return s, err
	}

	return makeScaledURL(s, scale), nil
}

// ImageThumbnail gets the thumbnail or direct URL of the image from the
// message.
func (c *Client) ImageThumbnail(msg *event.RoomMessageEvent, maxW, maxH, scale int) (string, error) {
	i, err := msg.ImageInfo()
	if err == nil {
		maxW, maxH = MaxSize(i.Width, i.Height, maxW, maxH)

		if i.ThumbnailURL != "" {
			return c.ScaledThumbnail(i.ThumbnailURL, maxW, maxH, scale)
		}
	}

	if msg.MessageType != event.RoomMessageImage {
		return "", errors.New("message is not image")
	}

	return c.ScaledThumbnail(msg.URL, maxW, maxH, scale)
}

// MaxSize returns the maximum size that can fit within the given max width and
// height. Aspect ratio is preserved.
func MaxSize(w, h, maxW, maxH int) (int, int) {
	if w == 0 {
		w = maxW
	}
	if h == 0 {
		h = maxH
	}
	if w < maxW && h < maxH {
		return w, h
	}

	if w > h {
		h = h * maxW / w
		w = maxW
	} else {
		w = w * maxH / h
		h = maxH
	}

	return w, h
}

// MessageMediaURL gets the message's media URL, if any.
func (c *Client) MessageMediaURL(msg *event.RoomMessageEvent) (string, error) {
	filename := msg.Body

	if filename == "" {
		i, err := msg.FileInfo()
		if err == nil {
			t, err := mime.ExtensionsByType(i.MimeType)
			if err == nil && t != nil {
				filename = "file" + t[0]
			}
		}
	}

	u, err := c.MediaDownloadURL(msg.URL, true, filename)
	if err != nil {
		return "", errors.Wrap(err, "failed to get download URL")
	}

	return u, nil
}

// RoomEvent queries the event with the given type. If the event type implies a
// state event, then the empty key is tried.
func (c *Client) RoomEvent(roomID matrix.RoomID, typ event.Type) (event.Event, error) {
	ev, _ := c.State.RoomEvent(roomID, typ)
	if ev != nil {
		return ev, nil
	}

	// wack
	return c.RoomState(roomID, typ, "")
}

// RoomState queries the internal State for the given RoomEvent. If the State
// does not have that event, it queries the homeserver directly.
func (c *Client) RoomState(
	roomID matrix.RoomID, typ event.Type, key string) (event.StateEvent, error) {

	s, err := c.State.RoomState(roomID, typ, key)
	if err == nil {
		return s, nil
	}

	raw, err := c.Client.Client.RoomState(roomID, typ, key)
	if err != nil {
		return nil, err
	}

	e, err := sys.ParseAs(raw, typ)
	if err != nil {
		return nil, err
	}

	stateEvent, ok := e.(event.StateEvent)
	if !ok {
		return nil, gotrix.ErrInvalidStateEvent
	}

	info := stateEvent.StateInfo()
	info.RoomID = roomID

	// Update the state cache for future calls.
	c.State.AddRoomEvents(roomID, []event.RawEvent{raw})

	return stateEvent, nil
}

// EachRoomState calls f on every raw event in the room state. It satisfies the
// EachRoomState method requirement inside gotrix.State, but most callers should
// not use this method, since there is no length information.
//
// Deprecated: Use EachRoomStateLen.
func (c *Client) EachRoomState(
	roomID matrix.RoomID, typ event.Type, f func(string, event.StateEvent) error) error {

	return c.EachRoomStateLen(roomID, typ, func(e event.StateEvent, _ int) error {
		return f(e.StateInfo().StateKey, e)
	})
}

// EachRoomStateLen is a variant of EachRoomState, but a length parameter is
// precalculated.
func (c *Client) EachRoomStateLen(
	roomID matrix.RoomID, typ event.Type, f func(ev event.StateEvent, total int) error) error {

	if err := c.State.EachRoomStateLen(roomID, typ, f); err == nil {
		return err
	}

	events, err := c.Client.RoomStates(roomID)
	if err != nil {
		return err
	}

	c.State.AddRoomEvents(roomID, events)

	return c.State.EachRoomStateLen(roomID, typ, f)
}

func (c *Client) RoomName(roomID matrix.RoomID) (string, error) {
	s, err := c.Client.RoomName(roomID)
	if err != nil {
		return s, err
	}

	if s == string(roomID) && c.ctx.Err() != nil {
		return s, c.ctx.Err()
	}

	return s, nil
}

// RoomType returns the room's type. An empty string signifies a regular room.
func (c *Client) RoomType(roomID matrix.RoomID) string {
	ev, _ := c.RoomEvent(roomID, event.TypeRoomCreate)
	if ev == nil {
		return ""
	}

	if ev.Info().Raw == nil {
		// No original JSON, so we can't get the Type field.
		return ""
	}

	var roomTypeEvent struct {
		Content struct {
			Type string
		}
	}

	json.Unmarshal(ev.Info().Raw, &roomTypeEvent)

	return roomTypeEvent.Content.Type
}

// RoomIsSpace returns true if the room with the given ID is a space-room.
func (c *Client) RoomIsSpace(roomID matrix.RoomID) bool {
	return c.RoomType(roomID) == "m.space"
}

// RoomIsUnread returns true if the room with the given ID has not been read by
// this user. The result of the unread boolean will always be valid, but if ok
// is false, then it might not be accurate.
func (c *Client) RoomIsUnread(roomID matrix.RoomID) (unread, ok bool) {
	t, err := c.RoomTimeline(roomID)
	if err != nil || len(t) == 0 {
		// Nothing in the timeline. Assume the user has already caught up, since
		// the room is empty.
		return false, false
	}

	seen, ok := c.hasSeenEvent(roomID, t[len(t)-1].RoomInfo().ID)
	return !seen, ok
}

func (c *Client) hasSeenEvent(roomID matrix.RoomID, eventID matrix.EventID) (seen, ok bool) {
	e, _ := c.RoomEvent(roomID, m.FullyReadEventType)
	if fullyRead, ok := e.(*m.FullyReadEvent); ok {
		// Assume that the user has caught up to the room if the latest event's
		// ID matches. Technically, there shouldn't ever be a case where the
		// fully read event would point to an event in the future, so this
		// should work.
		return fullyRead.EventID == eventID, true
	}

	u, err := c.Whoami()
	if err != nil {
		// Can't get the current user, so just assume that the room is unread.
		// This would be a bug, but whatever.
		return false, false
	}

	// Query to see if the current user has read the latest message.
	e, _ = c.RoomEvent(roomID, event.TypeReceipt)
	if e, ok := e.(*event.ReceiptEvent); ok {
		rc, ok := e.Events[eventID]
		if !ok {
			// Nobody has read the latest message, including the current user.
			return false, true
		}

		_, read := rc.Read[u]
		return read, true
	}

	return false, false
}

// RoomLatestReadEvent gets the latest read eventID. The event ID is an empty
// string if the user hasn't read anything.
func (c *Client) RoomLatestReadEvent(roomID matrix.RoomID) matrix.EventID {
	e, err := c.RoomEvent(roomID, m.FullyReadEventType)
	if err == nil {
		return e.(*m.FullyReadEvent).EventID
	}

	u, err := c.Whoami()
	if err != nil {
		return ""
	}

	e, err = c.RoomEvent(roomID, event.TypeReceipt)
	if err == nil {
		e := e.(*event.ReceiptEvent)

		for eventID, receipt := range e.Events {
			_, read := receipt.Read[u]
			if read {
				return eventID
			}
		}
	}

	return ""
}

// RoomCountUnread counts the number of unread events in a room. More is true if
// the user has never seen any of the messages in the room. The user should
// display that info as "${n}+" with the trailing plus.
func (c *Client) RoomCountUnread(roomID matrix.RoomID) (n int, more bool) {
	// empty ID is fine
	latestID := c.RoomLatestReadEvent(roomID)

	var unread int
	var found bool

	c.EachTimelineReverse(roomID, func(ev event.RoomEvent) error {
		if ev.RoomInfo().ID == latestID {
			found = true
			return EachBreak
		}
		unread++
		return nil
	})

	return unread, !found
}

// MarkRoomAsRead sends to the server that the current user has seen up to the
// given event in the given room.
func (c *Client) MarkRoomAsRead(roomID matrix.RoomID, eventID matrix.EventID) error {
	if seen, ok := c.hasSeenEvent(roomID, eventID); ok && seen {
		// Room is already seen; don't waste an API call.
		return nil
	}

	var request struct {
		FullyRead matrix.EventID `json:"m.fully_read"`
		Read      matrix.EventID `json:"m.read,omitempty"`
	}

	request.FullyRead = eventID
	request.Read = eventID

	return c.Request(
		"POST", api.EndpointRoom(roomID)+"/read_markers",
		nil, httputil.WithToken(), httputil.WithJSONBody(request),
	)
}

// RoomEnsureMembers ensures that the given room has all its members fetched.
func (c *Client) RoomEnsureMembers(roomID matrix.RoomID) error {
	const key = "ensure-members"

	if !c.State.SetRoom(roomID, key) {
		return nil
	}

	p, err := c.State.RoomPreviousBatch(roomID)
	if err != nil {
		c.State.ResetRoom(roomID, key)
		return fmt.Errorf("no previous_batch for room %q found", roomID)
	}

	e, err := c.Client.RoomMembers(roomID, api.RoomMemberFilter{
		At:         p,
		Membership: event.MemberJoined,
	})
	if err != nil {
		c.State.ResetRoom(roomID, key)
		return errors.Wrap(err, "failed to fetch members")
	}

	c.State.AddRoomEvents(roomID, e)

	batch := c.Index.Begin()
	defer batch.Commit()

	for _, raw := range e {
		me, ok := sys.ParseRoom(raw, roomID).(*event.RoomMemberEvent)
		if !ok {
			log.Printf("error: RoomMember event is of unexpected type %T", e)
			continue
		}

		batch.IndexRoomMember(me)
	}

	return nil
}

// RoomPaginator is used to fetch older messages from the API client.
type RoomPaginator struct {
	c      *Client
	roomID matrix.RoomID
	limit  int

	// buffer holds all the unreturned events.
	buffer []event.RoomEvent
	// lastEvID keeps track of the last event that the user received. Last here
	// means the earliest or top event.
	lastEvID matrix.EventID
	// lastBatch keeps track of the pagination token.
	lastBatch string
	// drained is true if the state cache is completely drained.
	drained bool
	// onTop is true if we're out of events.
	onTop bool
}

// RoomPaginator returns a new paginator that can fetch messages from the bottom
// up.
func (c *Client) RoomPaginator(roomID matrix.RoomID, limit int) *RoomPaginator {
	if limit < 1 {
		log.Panicln("gotktrix: RoomPaginator limit must be non-zero")
	}

	return &RoomPaginator{
		c:      c,
		limit:  limit,
		roomID: roomID,
	}
}

// TODO: this API is broken because the room list will shift messages over time.
// Paginate should take a message ID and repaginate if it cannot seek to the
// right position in the buffer.

// Paginate paginates from the client and the server if the database is drained.
func (p *RoomPaginator) Paginate(ctx context.Context) ([]event.RoomEvent, error) {
	if p.onTop {
		return nil, nil
	}

	// Fill the paginator's buffer until we have enough events in the buffer.
	if p.needFill() {
		if err := p.fill(ctx); err != nil {
			if len(p.buffer) == 0 {
				// Buffer completely drained. We got nothing.
				return nil, err
			}
			// If we get an error and a non-empty buffer, then that probably
			// means we're out of events in the room. Mark it as such and slice
			// the rest.
			p.onTop = true
			return p.buffer, nil
		}
	}

	// Calculate the boundary to which we should slice the buffer. The boundary
	// will be calculated starting from the end of buffer.
	bound := len(p.buffer)
	if bound > p.limit {
		bound -= p.limit
	}

	// Reslice the buffer to not have the region that we're about to split away.
	new := p.buffer[:bound]
	// Use all latest n=p.limit events.
	old := p.buffer[bound:]

	p.buffer = new
	return old, nil
}

// fill fills the paginator's buffer.
func (p *RoomPaginator) fill(ctx context.Context) error {
	if !p.drained {
		p.drained = true

		events, err := p.c.State.RoomTimeline(p.roomID)
		if err == nil {
			p.prepend(events)
			if !p.needFill() {
				return nil
			}
		}
	}

	if p.lastBatch == "" {
		// Acquire the latest known pagination token. This means we'll have to
		// seek through our cached events, but that's just how it works.
		b, err := p.c.State.RoomPreviousBatch(p.roomID)
		if err != nil {
			return errors.Wrap(err, "failed to get previous batch")
		}
		p.lastBatch = b
	}

	for p.needFill() {
		log.Println("from lastBatch =", p.lastBatch)

		// https://matrix.org/docs/spec/client_server/r0.6.1#get-matrix-client-r0-rooms-roomid-messages
		// Fill up the last batch from start.
		r, err := p.c.WithContext(ctx).RoomMessages(p.roomID, api.RoomMessagesQuery{
			From:      p.lastBatch,
			Direction: api.RoomMessagesBackward,
			Limit:     100,
		})
		if err != nil {
			return errors.Wrapf(err, "failed to query messages for room %q", p.roomID)
		}

		// If start and end matches, then we're out of messages.
		if r.Start == r.End {
			log.Println("no more messages")
			p.onTop = true
			break
		}

		// Update the last batch.
		// End is used to request earlier events if direction is backwards.
		p.lastBatch = r.End

		log.Println("new  lastBatch =", p.lastBatch)
		log.Println("     start =", r.Start)

		// Flip the message list. Code from SliceTricks.
		for i, j := 0, len(r.Chunk)-1; i < j; i, j = i+1, j-1 {
			r.Chunk[i], r.Chunk[j] = r.Chunk[j], r.Chunk[i]
		}

		// Seek until we stumble on the wanted events.
		events := sys.ParseAllTimeline(r.Chunk, p.roomID)
		for i, ev := range events {
			if ev.RoomInfo().ID == p.lastEvID {
				// Include all events from before the found one to the first
				// event, which is the earliest event.
				p.prepend(events[:i])
				log.Println("seeked to last event ID")
				break
			}
		}
		log.Println("cannot seek to last event ID")
	}

	return nil
}

// needFill returns true if the paginator's buffer needs filling.
func (p *RoomPaginator) needFill() bool {
	return p.limit > len(p.buffer) && !p.onTop
}

// prepend prepends the given events into the paginator buffer.
func (p *RoomPaginator) prepend(events []event.RoomEvent) {
	if len(p.buffer)+len(events) == 0 {
		p.buffer = nil
		return
	}

	// TODO: optimize.
	new := make([]event.RoomEvent, len(p.buffer)+len(events))

	n := 0
	n += copy(new[n:], events)
	n += copy(new[n:], p.buffer)

	p.buffer = new
	p.lastEvID = p.buffer[0].RoomInfo().ID // first is earliest
}

// RoomTimeline queries the state cache for the timeline of the given room. If
// it's not available, the API will be queried directly. The order of these
// events is guaranteed to be latest last.
func (c *Client) RoomTimeline(roomID matrix.RoomID) ([]event.RoomEvent, error) {
	if events, err := c.State.RoomTimeline(roomID); err == nil {
		return events, nil
	}

	// Obtain the previous batch.
	prev, err := c.State.RoomPreviousBatch(roomID)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get room previous batch")
	}

	// Re-check the state for the timeline, because we don't want to miss out
	// any events whil we were fetching the previous_batch string.
	if events, err := c.State.RoomTimeline(roomID); err == nil {
		return events, nil
	}

	r, err := c.RoomMessages(roomID, api.RoomMessagesQuery{
		From:      prev,
		Direction: api.RoomMessagesForward, // latest last
		Limit:     state.TimelineKeepLast,
	})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get messages for room %q", roomID)
	}

	return sys.ParseAllTimeline(r.Chunk, roomID), nil
}

// LatestMessage finds the latest room message event from the given list of
// events. The list is assumed to have the latest events last.
func LatestMessage(events []event.RoomEvent) *event.RoomMessageEvent {
	for i := len(events) - 1; i >= 0; i-- {
		if msg, ok := events[i].(*event.RoomMessageEvent); ok {
			return msg
		}
	}
	return nil
}

// AsyncSetConfig updates the state cache first, and then updates the API in the
// background.
//
// If done is given, then it's called once the API is updated. Most of the time,
// done should only be used to display errors; to know when things are updated,
// use a handler. Because of that, done may be invoked before AsyncConfigSet has
// been returned when there's an error. Done might also be called in a different
// goroutine.
func (c *Client) AsyncSetConfig(ev event.Event, done func(error)) {
	c.State.SetUserEvent(ev)

	go func() {
		err := c.ClientConfigSet(string(ev.Info().Type), ev)
		if done != nil {
			done(err)
		}
	}()
}

// UserEvent gets the user event from the state or the API.
func (c *Client) UserEvent(typ event.Type) (event.Event, error) {
	e, _ := c.State.UserEvent(typ)
	if e != nil {
		return e, nil
	}

	var raw json.RawMessage
	if err := c.ClientConfig(string(typ), &raw); err != nil {
		return nil, errors.Wrap(err, "failed to get client config")
	}

	e, err := sys.ParseUserEventContent(typ, raw)
	if err != nil {
		log.Printf("failed to parse UserEvent %s: %v", typ, err)
		return nil, errors.Wrap(err, "failed to parse event from API")
	}

	c.State.SetUserEvent(e)
	return e, nil
}

// Rooms returns the list of rooms the user is in.
func (c *Client) Rooms() ([]matrix.RoomID, error) {
	if roomIDs, err := c.State.Rooms(); err == nil {
		return roomIDs, nil
	}

	return c.Client.Rooms()
}

// RoomMembers returns a list of room members.
func (c *Client) RoomMembers(roomID matrix.RoomID) ([]event.RoomMemberEvent, error) {
	var events []event.RoomMemberEvent

	onEach := func(e event.StateEvent, total int) error {
		ev, ok := e.(*event.RoomMemberEvent)
		if !ok {
			return nil
		}

		if events == nil {
			events = make([]event.RoomMemberEvent, 0, total)
		}

		events = append(events, *ev)
		return nil
	}

	if err := c.State.EachRoomStateLen(roomID, event.TypeRoomMember, onEach); err == nil {
		if events != nil {
			return events, nil
		}
	}

	// prev is optional.
	prev, _ := c.State.RoomPreviousBatch(roomID)

	rawEvs, err := c.Client.RoomMembers(roomID, api.RoomMemberFilter{At: prev})
	if err != nil {
		return nil, errors.Wrap(err, "failed to query RoomMembers from API")
	}

	// Save the obtained events into the state cache.
	c.State.AddRoomEvents(roomID, rawEvs)

	events = make([]event.RoomMemberEvent, 0, len(rawEvs))

	for _, raw := range rawEvs {
		ev, ok := sys.ParseRoom(raw, roomID).(*event.RoomMemberEvent)
		if !ok {
			continue
		}

		events = append(events, *ev)
	}

	return events, nil
}

// EachTimeline iterates through the timeline.
func (c *Client) EachTimeline(roomID matrix.RoomID, f func(event.RoomEvent) error) error {
	return c.State.EachTimeline(roomID, f)
}

// EachTimelineReverse iterates through the timeline in reverse.
func (c *Client) EachTimelineReverse(roomID matrix.RoomID, f func(event.RoomEvent) error) error {
	return c.State.EachTimelineReverse(roomID, f)
}

// SendRoomEvent is a convenient function around RoomEventSend.
func (c *Client) SendRoomEvent(roomID matrix.RoomID, ev event.Event) error {
	if ev.Info().Type == "" {
		// bug
		panic("SendRoomEvent: missing event type")
	}

	_, err := c.Client.RoomEventSend(roomID, ev.Info().Type, ev)
	return err
}

// Redact redacts a room event.
func (c *Client) Redact(roomID matrix.RoomID, ev matrix.EventID, reason string) error {
	_, err := c.RoomEventRedact(roomID, ev, reason)
	return err
}

// PowerAction describes 1 out of the 4 actions in a PowerLevels event.
type PowerAction uint8

const (
	_ PowerAction = iota
	BanAction
	InviteAction
	KickAction
	RedactAction
)

// HasPower checks if the current user can perform the given action inside the
// given room.
func (c *Client) HasPower(roomID matrix.RoomID, action PowerAction) bool {
	e, err := c.RoomState(roomID, event.TypeRoomPowerLevels, "")
	if err != nil {
		// Theoretically, this means we have the power to override the room's
		// power levels to be whatever we want, but we'll play nice and pretend
		// that we don't have the power to do that, because that's just stupid.
		return false
	}

	ev := e.(*event.RoomPowerLevelsEvent)

	powerLevel := 50

	switch action {
	case BanAction:
		if ev.BanRequirement != nil {
			powerLevel = *ev.BanRequirement
		}
	case InviteAction:
		if ev.InviteRequirement != nil {
			powerLevel = *ev.InviteRequirement
		}
	case KickAction:
		if ev.KickRequirement != nil {
			powerLevel = *ev.KickRequirement
		}
	case RedactAction:
		if ev.RedactRequirement != nil {
			powerLevel = *ev.RedactRequirement
		}
	}

	ourLevel := ev.UserDefault
	if level, ok := ev.UserLevel[c.UserID]; ok {
		ourLevel = level
	}

	if ourLevel >= powerLevel {
		return true
	}

	if c.IsRoomCreator(roomID) {
		// User made this room, so they have full power.
		return true
	}

	return false
}

// IsRoomCreator returns true if the current user is the user who made this
// room.
func (c *Client) IsRoomCreator(roomID matrix.RoomID) bool {
	e, err := c.RoomState(roomID, event.TypeRoomCreate, "")
	if err != nil {
		return false
	}
	return e.(*event.RoomCreateEvent).Creator == c.UserID
}

// MemberName describes a member name.
type MemberName struct {
	Name      string
	Ambiguous bool
}

// MemberName calculates the display name of a member. Note that a user joining
// might invalidate some names if they share the same display name as
// disambiguation will become necessary.
//
// Use the Client.MemberNames variant when generating member name for multiple
// users to reduce duplicate work.
//
// If check is true, then the MemberName's Ambiguous field will be set to true
// if the display name collides with someone else's. This check is quite
// expensive, so it should only be enabled when needed.
func (c *Client) MemberName(
	roomID matrix.RoomID, userID matrix.UserID, check bool) (MemberName, error) {

	names, err := c.memberNames(roomID, []matrix.UserID{userID}, check)
	if err != nil {
		return MemberName{}, err
	}

	return names[0], nil
}

// memberNames calculates the display name of all the users provided.
func (c *Client) memberNames(
	roomID matrix.RoomID, userIDs []matrix.UserID, check bool) ([]MemberName, error) {

	results := make([]MemberName, len(userIDs))

	for i, userID := range userIDs {
		e, _ := c.RoomState(roomID, event.TypeRoomMember, string(userID))
		if e == nil {
			results[i].Name = string(userID)
			continue
		}

		memberEvent := e.(*event.RoomMemberEvent)
		if memberEvent.DisplayName == nil || *memberEvent.DisplayName == "" {
			results[i].Name = string(userID)
			continue
		}

		results[i].Name = *memberEvent.DisplayName
	}

	if !check {
		return results, nil
	}

	for i, result := range results {
		for _, userID := range c.State.RoomMembersFromName(roomID, result.Name) {
			if userID != userIDs[i] {
				results[i].Ambiguous = true
			}
		}
	}

	return results, nil
}

// UpdateRoomTags updates the internal state with the latest room tag
// information.
func (c *Client) UpdateRoomTags(roomID matrix.RoomID) error {
	t, err := c.Client.Tags(roomID)
	if err != nil {
		return err
	}

	b, err := json.Marshal(event.TagEvent{Tags: t})
	if err != nil {
		return errors.Wrap(err, "failed to marshal room tags")
	}

	c.State.AddRoomEvents(roomID, []event.RawEvent{
		sys.MarshalUserEvent(event.TypeTag, b),
	})

	return nil
}

// IsDirect returns true if the given room is a direct messaging room.
func (c *Client) IsDirect(roomID matrix.RoomID) bool {
	if is, ok := c.State.IsDirect(roomID); ok {
		return is
	}

	if e, err := c.Client.DMRooms(); err == nil {
		c.State.UseDirectEvent(e)
		return roomIsDM(e, roomID)
	}

	u, err := c.Whoami()
	if err != nil {
		return false
	}

	// Resort to querying the room state directly from the API. State.IsDirect
	// already queries RoomState on itself, so we don't need to do that.
	r, err := c.Client.Client.RoomState(roomID, event.TypeRoomMember, string(u))
	if err != nil {
		return false
	}

	// Save the event we've fetched into the state.
	c.State.AddRoomEvents(roomID, []event.RawEvent{r})

	e, err := sys.ParseAs(r, event.TypeRoomMember)
	if err != nil {
		return false
	}

	ev, _ := e.(*event.RoomMemberEvent)
	return ev.IsDirect
}

func roomIsDM(dir *event.DirectEvent, roomID matrix.RoomID) bool {
	for _, ids := range dir.Rooms {
		for _, id := range ids {
			if id == roomID {
				return true
			}
		}
	}
	return false
}
