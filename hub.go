package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ==== Matchmaking + room management ====
//
// Wants (memory se): sirf same bet-amount wale real online players match
// hon; agar match na mile to player ko waiting mein hi rakho, timeout laga
// kar kisi bot/random se match mat karo.

// ClientMsg — client se aane wale saare messages isi shape mein aate hain.
type ClientMsg struct {
	Type     string `json:"type"`
	Email    string `json:"email"`
	Password string `json:"password"`
	PlayerID string `json:"player_id"`
	Name     string `json:"name"`
	Avatar   string `json:"avatar"`
	Bet      int    `json:"bet"`
	Mode     string `json:"mode"`
	Players  int    `json:"players"` // 2 ya 4
	Magic    bool   `json:"magic"`
	Token    int    `json:"token"`
	Value    int    `json:"value"`
}

// ProfileInfo — display name + hosted avatar URL (kabhi bhi base64/data-URI
// nahi hota, hamesha ek https link jo /avatar upload endpoint se milta hai).
type ProfileInfo struct {
	Name   string `json:"name"`
	Avatar string `json:"avatar,omitempty"`
}

// ServerMsg — server se client ko jane wale saare messages.
type ServerMsg struct {
	Type      string                 `json:"type"`
	PlayerID  string                 `json:"player_id,omitempty"`
	AuthToken string                 `json:"auth_token,omitempty"`
	RoomID    string                 `json:"room_id,omitempty"`
	Color     Color                  `json:"color,omitempty"`
	Players   []Color                `json:"players,omitempty"`
	Mode      Mode                   `json:"mode,omitempty"`
	Bet       int                    `json:"bet,omitempty"`
	Coins     int64                  `json:"coins,omitempty"`
	Diamonds  int64                  `json:"diamonds,omitempty"`
	State     *Snapshot              `json:"state,omitempty"`
	Events    []Event                `json:"events,omitempty"`
	Message   string                 `json:"message,omitempty"`
	Name      string                 `json:"name,omitempty"`
	Avatar    string                 `json:"avatar,omitempty"`
	Profiles  map[Color]ProfileInfo  `json:"profiles,omitempty"`
	Seconds   int                    `json:"seconds,omitempty"`
}

type Client struct {
	id     string
	name   string // profile display name (email nahi — opponent ko yehi dikhta hai)
	avatar string // hosted avatar URL (kabhi base64 nahi)
	conn   *websocket.Conn
	send   chan []byte

	mu        sync.Mutex
	authed    bool
	room      *Room
	color     Color
	closeOnce sync.Once
	closed    bool
}

// closeSend — channel ko safely (sirf ek dafa) close karta hai, chahe yeh
// kitni bhi jaghon se call ho (slow-client drop + normal disconnect dono).
// "closed" flag age ki har sendJSON call ko closed channel par bhejne se rokta hai.
func (c *Client) closeSend() {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		close(c.send)
	})
}

func (c *Client) writePump() {
	for msg := range c.send {
		c.mu.Lock()
		err := c.conn.WriteMessage(websocket.TextMessage, msg)
		c.mu.Unlock()
		if err != nil {
			return
		}
	}
}

func (c *Client) sendJSON(v interface{}) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	select {
	case c.send <- b:
	default:
		// client bahut slow hai / buffer bhar chuka — connection drop hone dete hain
		go c.closeSend()
	}
}

const TurnTimeoutSeconds = 12

type Room struct {
	id      string
	mode    Mode
	bet     int
	game    *GameState
	clients map[Color]*Client
	mu      sync.Mutex

	// Turn timer — har turn shuru hote hi 12s ka countdown arm hota hai; agar
	// player us waqt tak roll/move na kare to server khud us ki taraf se
	// action le leta hai (auto-play), taake game kabhi hamesha ke liye na atke.
	timerMu  sync.Mutex
	timer    *time.Timer
	timerSeq int
}

func (r *Room) broadcast(msg ServerMsg) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.clients {
		c.sendJSON(msg)
	}
}

func (r *Room) broadcastExcept(exclude Color, msg ServerMsg) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for color, c := range r.clients {
		if color == exclude {
			continue
		}
		c.sendJSON(msg)
	}
}

type waitKey struct {
	mode    string
	bet     int
	players int
	magic   bool
}

type Hub struct {
	mu      sync.Mutex
	waiting map[waitKey][]*Client
	rooms   map[string]*Room
	roomSeq int
	store   *Store

	// activeByID — "1 ID sirf 1 device" ka enforcement: har account ke liye
	// sirf ek hi live connection allowed hai. Naya login/signup aane par purana
	// connection turant force-logout ho jata hai.
	activeByID map[string]*Client
}

func NewHub(store *Store) *Hub {
	return &Hub{
		waiting:    map[waitKey][]*Client{},
		rooms:      map[string]*Room{},
		store:      store,
		activeByID: map[string]*Client{},
	}
}

// setActive — is account ID ko is client se "current device" mark karta hai.
// Agar koi purana connection isi ID par pehle se active tha, wo wapis kar deta
// hai taake use force-logout kiya ja sake.
func (h *Hub) setActive(id string, c *Client) *Client {
	h.mu.Lock()
	old := h.activeByID[id]
	h.activeByID[id] = c
	h.mu.Unlock()
	if old != nil && old != c {
		return old
	}
	return nil
}

func (h *Hub) clearActive(id string, c *Client) {
	h.mu.Lock()
	if h.activeByID[id] == c {
		delete(h.activeByID, id)
	}
	h.mu.Unlock()
}

// kickOld — purane device wale connection ko batata hai ke kisi aur jagah se
// login ho gaya hai, phir uska socket band kar deta hai (agar wo kisi game/queue
// mein ho to wahan se bhi nikal deta hai).
func (h *Hub) kickOld(old *Client) {
	old.sendJSON(ServerMsg{Type: "forceLogout", Message: "aapki ID kisi doosre phone/device par login ho gayi hai — is device se logout kiya ja raha hai"})
	// Sirf connection band karte hain — ReadPump ka apna defer (LeaveQueue/
	// LeaveRoom/clearActive) khud chal jayega jaise normal disconnect par hota
	// hai. Yahan khud LeaveRoom call karna double-forfeiture jaisa bug bana
	// sakta tha (coins do dafa credit hone ka khatra).
	old.closeSend()
	old.conn.Close()
}

// Join — client ko matchmaking queue mein daalta hai. Same mode+bet+player-count
// wale kaafi log jama hone par turant room bana kar sabko "matched" bhej deta hai.
// Queue mein daalne se pehle balance check hota hai — bet se kam coins hon to
// join hi nahi hone dete.
func (h *Hub) Join(c *Client, mode string, bet int, playerCount int, magic bool) {
	if bet <= 0 {
		c.sendJSON(ServerMsg{Type: "error", Message: "bet amount ghalat hai"})
		return
	}
	coins, err := h.store.GetCoins(c.id)
	if err != nil {
		c.sendJSON(ServerMsg{Type: "error", Message: "account nahi mila"})
		return
	}
	if coins < int64(bet) {
		c.sendJSON(ServerMsg{Type: "error", Message: "coins kam hain is bet ke liye", Coins: coins})
		return
	}

	if playerCount != 2 && playerCount != 4 {
		playerCount = 2
	}
	m := Mode(mode)
	switch m {
	case ModeClassic, ModeArrow, ModeQuick, ModeMaster:
	default:
		m = ModeClassic
	}

	key := waitKey{mode: string(m), bet: bet, players: playerCount, magic: magic}

	h.mu.Lock()
	h.waiting[key] = append(h.waiting[key], c)
	queue := h.waiting[key]

	if len(queue) < playerCount {
		h.mu.Unlock()
		c.sendJSON(ServerMsg{Type: "waiting", Message: fmt.Sprintf("%d/%d players — same bet ka koi aur player dhoond rahe hain", len(queue), playerCount)})
		return
	}

	// poori table mil gayi — queue se nikal kar room banao
	group := queue[:playerCount]
	h.waiting[key] = queue[playerCount:]
	h.roomSeq++
	roomID := fmt.Sprintf("room-%d", h.roomSeq)
	h.mu.Unlock()

	colors := PlayerColors2P
	if playerCount == 4 {
		colors = PlayerColors4P
	}

	game := NewGameState(m, colors, magic)
	room := &Room{id: roomID, mode: m, bet: bet, game: game, clients: map[Color]*Client{}}

	for i, member := range group {
		color := colors[i]
		member.mu.Lock()
		member.room = room
		member.color = color
		member.mu.Unlock()
		room.clients[color] = member
	}

	h.mu.Lock()
	h.rooms[roomID] = room
	h.mu.Unlock()

	// Sab members ke naam/dp ek dafa jama kar lo — sab players ko "matched"
	// message ke sath dusron ki profile bhi mil jaye (opponent ka naam+dp
	// dikhane ke liye).
	profiles := map[Color]ProfileInfo{}
	for _, member := range group {
		member.mu.Lock()
		profiles[member.color] = ProfileInfo{Name: member.name, Avatar: member.avatar}
		member.mu.Unlock()
	}

	// Sabki bet ek sath escrow mein kaat lo — har player ko apna naya (post-deduction) balance milta hai.
	for _, member := range group {
		newBal, dErr := h.store.AdjustCoins(member.id, -int64(bet))
		if dErr != nil {
			newBal, _ = h.store.GetCoins(member.id)
			member.sendJSON(ServerMsg{Type: "error", Message: "bet deduct nahi ho saki: " + dErr.Error(), Coins: newBal})
			continue
		}
		member.sendJSON(ServerMsg{
			Type:     "matched",
			RoomID:   roomID,
			Color:    member.color,
			Players:  colors,
			Mode:     m,
			Bet:      bet,
			Coins:    newBal,
			State:    game.Snapshot(),
			Profiles: profiles,
		})
	}

	// Pehle turn ke liye 12-second countdown shuru kar dete hain.
	h.armTurnTimer(room)
}

// LeaveQueue — agar client disconnect ho jaye jabke abhi match nahi mila.
func (h *Hub) LeaveQueue(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for key, q := range h.waiting {
		for i, m := range q {
			if m == c {
				h.waiting[key] = append(q[:i], q[i+1:]...)
				return
			}
		}
	}
}

// LeaveRoom — game ke doraan koi disconnect/leave ho jaye to baaki players ko
// batata hai, aur usay "hataa hua" treat karta hai: agar bet wali game thi to
// uske coins turant baaki bache hue player(s) ki taraf chale jate hain (2-player
// game mein poora pot mil jata hai aur game khatam ho jati hai; 4-player mein
// sirf iski bet ka hissa baante hue baaki players ko mil jata hai aur game
// jaari rehti hai — us player ki taraf ke turns turn-timer khud auto-play
// karta rehta hai).
func (h *Hub) LeaveRoom(c *Client) {
	c.mu.Lock()
	room := c.room
	color := c.color
	c.room = nil
	c.mu.Unlock()
	if room == nil {
		return
	}

	room.mu.Lock()
	_, wasIn := room.clients[color]
	delete(room.clients, color)
	remaining := make([]*Client, 0, len(room.clients))
	for _, rc := range room.clients {
		remaining = append(remaining, rc)
	}
	room.mu.Unlock()

	if !wasIn {
		return // already leave ho chuka tha (duplicate call) — kuch dobara mat karo
	}

	room.broadcastExcept(color, ServerMsg{Type: "opponentLeft", Color: color})

	if room.bet > 0 && !room.game.IsOver() {
		if len(remaining) == 1 {
			// Sirf ek player bacha — usay turant winner declare kar ke poora pot de do.
			winner := remaining[0]
			pot := int64(room.bet) * int64(len(room.game.Players))
			room.game.ForceEnd(winner.color)
			newBal, err := h.store.AdjustCoins(winner.id, pot)
			if err == nil {
				winner.sendJSON(ServerMsg{Type: "wallet", Color: winner.color, Coins: newBal, Message: "opponent left — aap jeet gaye, pot credit ho gaya"})
			}
			room.timerMu.Lock()
			if room.timer != nil {
				room.timer.Stop()
				room.timer = nil
			}
			room.timerMu.Unlock()
		} else if len(remaining) > 1 {
			// 4-player game mein — jaane wale ki bet baaki bache hue players mein
			// barabar baant do, game jaari rehti hai.
			share := int64(room.bet) / int64(len(remaining))
			if share > 0 {
				for _, rc := range remaining {
					newBal, err := h.store.AdjustCoins(rc.id, share)
					if err == nil {
						rc.sendJSON(ServerMsg{Type: "wallet", Color: rc.color, Coins: newBal, Message: fmt.Sprintf("%s left — unki bet ka hissa mil gaya", color)})
					}
				}
			}
		}
	}

	if len(remaining) == 0 {
		h.mu.Lock()
		delete(h.rooms, room.id)
		h.mu.Unlock()
		room.timerMu.Lock()
		if room.timer != nil {
			room.timer.Stop()
			room.timer = nil
		}
		room.timerMu.Unlock()
	}
}

// armTurnTimer — jis player ki taraf se action (roll ya move) expect ho raha
// hai uske liye 12-second countdown (re)start karta hai aur room ko "turnTimer"
// bhejta hai (isi se client us player ki profile par ring/countdown dikhata
// hai). Timeout par khud hi us player ki taraf se action le leta hai
// (auto-play) taake koi bhi inactive player game ko atka na sake.
func (h *Hub) armTurnTimer(room *Room) {
	pending := room.game.PendingAction()

	room.timerMu.Lock()
	if room.timer != nil {
		room.timer.Stop()
		room.timer = nil
	}
	if pending == nil {
		room.timerMu.Unlock()
		return
	}
	room.timerSeq++
	seq := room.timerSeq
	room.timer = time.AfterFunc(TurnTimeoutSeconds*time.Second, func() {
		h.onTurnTimeout(room, seq, pending)
	})
	room.timerMu.Unlock()

	room.broadcast(ServerMsg{Type: "turnTimer", Color: pending.Color, Seconds: TurnTimeoutSeconds})
}

func (h *Hub) onTurnTimeout(room *Room, seq int, pending *PendingAction) {
	room.timerMu.Lock()
	if seq != room.timerSeq {
		room.timerMu.Unlock()
		return // is dauran player ne khud action le liya ya kuch aur ho gaya — yeh timer purana hai
	}
	room.timerMu.Unlock()

	var events []Event
	var err error
	if pending.Kind == "move" {
		events, err = room.game.MoveToken(pending.Color, pending.Movable[0], 0)
	} else {
		events, err = room.game.RollDice(pending.Color)
	}
	if err != nil {
		return
	}
	room.broadcast(ServerMsg{Type: "events", Events: events, State: room.game.Snapshot(), Message: "player inactive tha — server ne uski taraf se auto-play kiya"})
	h.settleGameOver(room, events)
	h.armTurnTimer(room)
}

// settleGameOver — agar events mein "gameOver" ho to poora pot (sab players ki
// bet ka jama) jeetne wale ke account mein credit kar deta hai aur sab clients
// ko un ka updated balance batata hai.
func (h *Hub) settleGameOver(room *Room, events []Event) {
	for _, e := range events {
		if e.Type != "gameOver" {
			continue
		}
		room.mu.Lock()
		winnerClient, ok := room.clients[e.Winner]
		room.mu.Unlock()
		if !ok {
			continue
		}
		pot := int64(room.bet) * int64(len(room.game.Players))
		newBal, err := h.store.AdjustCoins(winnerClient.id, pot)
		if err != nil {
			log.Println("settleGameOver: credit failed:", err)
			continue
		}
		winnerClient.sendJSON(ServerMsg{Type: "wallet", Color: e.Winner, Coins: newBal, Message: "aap jeet gaye! pot credit ho gaya"})
		room.broadcastExcept(e.Winner, ServerMsg{Type: "wallet", Color: e.Winner, Message: fmt.Sprintf("%s jeet gaya, pot le gaya", e.Winner)})
	}
}

func (h *Hub) handleRoll(c *Client) {
	c.mu.Lock()
	room := c.room
	color := c.color
	c.mu.Unlock()
	if room == nil {
		c.sendJSON(ServerMsg{Type: "error", Message: "not in a game yet"})
		return
	}
	events, err := room.game.RollDice(color)
	if err != nil {
		c.sendJSON(ServerMsg{Type: "error", Message: err.Error()})
		return
	}
	room.broadcast(ServerMsg{Type: "events", Events: events, State: room.game.Snapshot()})
	h.settleGameOver(room, events)
	h.armTurnTimer(room)
}

func (h *Hub) handleMove(c *Client, tokenIdx int, value int) {
	c.mu.Lock()
	room := c.room
	color := c.color
	c.mu.Unlock()
	if room == nil {
		c.sendJSON(ServerMsg{Type: "error", Message: "not in a game yet"})
		return
	}
	events, err := room.game.MoveToken(color, tokenIdx, value)
	if err != nil {
		c.sendJSON(ServerMsg{Type: "error", Message: err.Error()})
		return
	}
	room.broadcast(ServerMsg{Type: "events", Events: events, State: room.game.Snapshot()})
	h.settleGameOver(room, events)
	h.armTurnTimer(room)
}

// handleBuyExtraRoll — client "buyExtraRoll" bhejta hai (koi extra field nahi
// chahiye — cost server khud current player ke count/spent se nikalta hai).
// Diamonds pehle deduct hote hain, tabhi dice roll hota hai; agar kisi wajah
// se roll fail ho (turn nikal chuka waghera) to diamonds refund ho jate hain.
func (h *Hub) handleBuyExtraRoll(c *Client) {
	c.mu.Lock()
	room := c.room
	color := c.color
	c.mu.Unlock()
	if room == nil {
		c.sendJSON(ServerMsg{Type: "error", Message: "not in a game yet"})
		return
	}

	cost := room.game.NextExtraRollCost(color)
	if cost == 0 {
		c.sendJSON(ServerMsg{Type: "error", Message: "is game mein extra-roll ki 1000 diamond limit khatam ho chuki — lock ho gaya"})
		return
	}

	newDiamonds, err := h.store.AdjustDiamonds(c.id, -cost)
	if err != nil {
		c.sendJSON(ServerMsg{Type: "error", Message: "diamonds kam hain extra-roll ke liye"})
		return
	}

	events, err := room.game.BuyExtraRoll(color, cost)
	if err != nil {
		// roll fail hui (jaise turn nikal chuka) — kate hue diamonds wapis kar do
		refunded, _ := h.store.AdjustDiamonds(c.id, cost)
		c.sendJSON(ServerMsg{Type: "error", Message: err.Error(), Diamonds: refunded})
		return
	}

	c.sendJSON(ServerMsg{Type: "wallet", Diamonds: newDiamonds, Message: "extra roll khareeda"})
	room.broadcast(ServerMsg{Type: "events", Events: events, State: room.game.Snapshot()})
	h.settleGameOver(room, events)
	h.armTurnTimer(room)
}

// handleSignup / handleLogin — asal HTTP endpoints ki jagah ab yeh seedha
// WebSocket message se hota hai. Kaamyabi par client "authed" ho jata hai aur
// tabhi "join"/"roll"/"move" bhej sakta hai — is se pehle koi bhi HTTP request
// nahi lagti, sab isi socket ke andar.
func (h *Hub) handleSignup(c *Client, email, password string) {
	if email == "" || password == "" {
		c.sendJSON(ServerMsg{Type: "error", Message: "email aur password dono zaroori hain"})
		return
	}
	if len(password) < 6 {
		c.sendJSON(ServerMsg{Type: "error", Message: "password kam se kam 6 characters ka ho"})
		return
	}
	acc, token, err := h.store.SignUp(email, password)
	if err != nil {
		c.sendJSON(ServerMsg{Type: "error", Message: err.Error()})
		return
	}
	c.mu.Lock()
	c.id = acc.ID
	c.name = acc.Name
	c.avatar = acc.Avatar
	c.authed = true
	c.mu.Unlock()
	if old := h.setActive(acc.ID, c); old != nil {
		h.kickOld(old)
	}
	c.sendJSON(ServerMsg{Type: "auth", PlayerID: acc.ID, AuthToken: token, Coins: acc.Coins, Diamonds: acc.Diamonds, Name: acc.Name, Avatar: acc.Avatar})
}

func (h *Hub) handleLogin(c *Client, email, password string) {
	acc, token, err := h.store.Login(email, password)
	if err != nil {
		c.sendJSON(ServerMsg{Type: "error", Message: err.Error()})
		return
	}
	c.mu.Lock()
	c.id = acc.ID
	c.name = acc.Name
	c.avatar = acc.Avatar
	c.authed = true
	c.mu.Unlock()
	// Isi ID se pehle koi aur device connected ho to usay turant nikaal do —
	// "1 ID sirf 1 device par" ka rule yahin lagu hota hai.
	if old := h.setActive(acc.ID, c); old != nil {
		h.kickOld(old)
	}
	c.sendJSON(ServerMsg{Type: "auth", PlayerID: acc.ID, AuthToken: token, Coins: acc.Coins, Diamonds: acc.Diamonds, Name: acc.Name, Avatar: acc.Avatar})
}

// handleUpdateProfile — client "updateProfile" bhejta hai jab user apna naam
// ya avatar badalta hai. Avatar hamesha ek hosted https URL hona chahiye
// (jaise /avatar upload endpoint deta hai) — base64/data-URI yahan reject ho
// jata hai, taake DB mein kabhi bhi image ka raw base64 save na ho.
func (h *Hub) handleUpdateProfile(c *Client, name, avatar string) {
	name = strings.TrimSpace(name)
	if name == "" {
		c.sendJSON(ServerMsg{Type: "error", Message: "naam khali nahi ho sakta"})
		return
	}
	if len(name) > 16 {
		name = name[:16]
	}
	if avatar != "" {
		if strings.HasPrefix(avatar, "data:") || len(avatar) > 500 {
			c.sendJSON(ServerMsg{Type: "error", Message: "avatar sirf ek image URL ho sakta hai (base64 allowed nahi) — pehle POST /avatar par photo upload karein, phir wapis mila hua URL yahan bhejein"})
			return
		}
		if !strings.HasPrefix(avatar, "http://") && !strings.HasPrefix(avatar, "https://") {
			c.sendJSON(ServerMsg{Type: "error", Message: "avatar ek valid http(s) URL hona chahiye"})
			return
		}
	}
	if err := h.store.UpdateProfile(c.id, name, avatar); err != nil {
		c.sendJSON(ServerMsg{Type: "error", Message: "profile save nahi ho saka"})
		return
	}
	c.mu.Lock()
	c.name = name
	c.avatar = avatar
	room := c.room
	color := c.color
	c.mu.Unlock()
	c.sendJSON(ServerMsg{Type: "profile", Name: name, Avatar: avatar})
	if room != nil {
		// Agar abhi kisi game mein hain to opponent ko turant naya naam/dp bata dein.
		room.broadcastExcept(color, ServerMsg{Type: "opponentProfile", Color: color, Name: name, Avatar: avatar})
	}
}

// ReadPump — client se aane wale messages parse karta hai aur route karta hai.
func (h *Hub) ReadPump(c *Client) {
	defer func() {
		c.mu.Lock()
		id := c.id
		c.mu.Unlock()
		if id != "" {
			h.clearActive(id, c)
		}
		h.LeaveQueue(c)
		h.LeaveRoom(c)
		c.closeSend()
		c.conn.Close()
	}()

	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		var msg ClientMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			c.sendJSON(ServerMsg{Type: "error", Message: "bad message format"})
			continue
		}

		// signup/login se pehle kuch aur allow nahi — baaki sab isi state par depend karta hai
		if msg.Type == "signup" {
			h.handleSignup(c, msg.Email, msg.Password)
			continue
		}
		if msg.Type == "login" {
			h.handleLogin(c, msg.Email, msg.Password)
			continue
		}

		c.mu.Lock()
		authed := c.authed
		c.mu.Unlock()
		if !authed {
			c.sendJSON(ServerMsg{Type: "error", Message: "pehle signup ya login karein"})
			continue
		}

		switch msg.Type {
		case "join":
			h.Join(c, msg.Mode, msg.Bet, msg.Players, msg.Magic)
		case "roll":
			h.handleRoll(c)
		case "move":
			h.handleMove(c, msg.Token, msg.Value)
		case "buyExtraRoll":
			h.handleBuyExtraRoll(c)
		case "updateProfile":
			h.handleUpdateProfile(c, msg.Name, msg.Avatar)
		case "leave":
			h.LeaveQueue(c)
			h.LeaveRoom(c)
		default:
			log.Printf("unknown message type from %s: %s", c.id, msg.Type)
		}
	}
}
