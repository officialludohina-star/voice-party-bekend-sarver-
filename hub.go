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
	// AuthToken — sirf "resume" message ke liye (naam "token" se isliye alag rakha
	// hai kyunke "token" field upar move ke tokenIndex ke liye pehle se use ho raha hai).
	AuthToken string `json:"auth_token"`
	// Otp — "resetPassword" ke sath server-verified 6-digit code (requestPasswordReset
	// se milta hai). Bina is match ke password kabhi reset nahi hota.
	Otp string `json:"otp"`
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

const TurnTimeoutSeconds = 10

// ==== Net/connection drop ke waqt reconnect grace ====
// Agar bet wali game ke doraan kisi ka connection (net jaane se) achanak toot
// jaye, to usay foran haara hua treat nahi karte — bilkul turant forfeit karne
// ki bajaye ReconnectGraceSeconds tak room reserved rakhte hain. Isi dauran
// agar player wapis connect ho kar apna purana auth_token "resume" message mein
// bhej de, to bilkul wahi room/color/game state wapis mil jati hai (poori
// Snapshot ke sath) — jaise kuch hua hi na ho. Agar poore grace period tak
// koi resume na aaye, tabhi jaake wahi purani forfeiture logic (LeaveRoom)
// chalti hai — dusre player ka intezaar hamesha ke liye nahi hota.
//
// Frontend apni taraf se "Reconnecting… 30s" dikha sakta hai aur 30s guzarne
// par "Exit / Connect" popup de sakta hai — "Connect" par bhi yehi resume flow
// dobara try hota hai, jab tak neeche wala poora grace window khatam na ho.
const ReconnectGraceSeconds = 30

type graceEntry struct {
	room  *Room
	color Color
	timer *time.Timer
}

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

	// grace — account ID -> uska pending reconnect (net drop ke baad, forfeit
	// hone se pehle ka window). Isi se "resume" message par pata chalta hai ke
	// yeh player kis room/color mein wapis jayega.
	grace map[string]*graceEntry

	// otps — forgot-password OTP ab yahan (server par) generate + verify hoti
	// hai, client par nahi (otp.go dekhein).
	otps *OtpStore
}

func NewHub(store *Store) *Hub {
	return &Hub{
		waiting:    map[waitKey][]*Client{},
		rooms:      map[string]*Room{},
		store:      store,
		activeByID: map[string]*Client{},
		grace:      map[string]*graceEntry{},
		otps:       NewOtpStore(),
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
	old.sendJSON(ServerMsg{Type: "forceLogout", Message: "your ID was just logged in on another phone/device — logging out this device"})
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
		c.sendJSON(ServerMsg{Type: "error", Message: "invalid bet amount"})
		return
	}
	coins, err := h.store.GetCoins(c.id)
	if err != nil {
		c.sendJSON(ServerMsg{Type: "error", Message: "account not found"})
		return
	}
	if coins < int64(bet) {
		c.sendJSON(ServerMsg{Type: "error", Message: "not enough coins for this bet", Coins: coins})
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

	// Sabki bet ek sath escrow mein kaat lo. Agar kisi ek ki deduction fail ho
	// jaye (jaise iske balance mein is dauran hi kami aa gayi ho), to poora match
	// cancel kar dete hain aur jin ki bet kat chuki thi unhein turant refund kar
	// dete hain — koi bhi player bina bet kate is room ke andar nahi reh sakta
	// (pehle yahan bug tha: fail hone par sirf error bhej dete thay lekin member
	// room.clients mein reh jata tha aur bina paise diye khel sakta tha).
	deductedBal := map[Color]int64{}
	anyFailed := false
	for _, member := range group {
		newBal, dErr := h.store.AdjustCoins(member.id, -int64(bet))
		if dErr != nil {
			anyFailed = true
			newBal, _ = h.store.GetCoins(member.id)
			member.sendJSON(ServerMsg{Type: "error", Message: "could not deduct bet: " + dErr.Error(), Coins: newBal})
			continue
		}
		deductedBal[member.color] = newBal
	}

	if anyFailed {
		for _, member := range group {
			if _, ok := deductedBal[member.color]; ok {
				refunded, _ := h.store.AdjustCoins(member.id, int64(bet))
				member.sendJSON(ServerMsg{Type: "error", Message: "match cancelled (one player's bet could not be deducted) — please try again", Coins: refunded})
			}
			member.mu.Lock()
			member.room = nil
			member.mu.Unlock()
		}
		h.mu.Lock()
		delete(h.rooms, roomID)
		h.mu.Unlock()
		return
	}

	for _, member := range group {
		member.sendJSON(ServerMsg{
			Type:     "matched",
			RoomID:   roomID,
			Color:    member.color,
			Players:  colors,
			Mode:     m,
			Bet:      bet,
			Coins:    deductedBal[member.color],
			State:    game.Snapshot(),
			Profiles: profiles,
		})
	}

	// Pehle turn ke liye 10-second countdown shuru kar dete hain.
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

			// Asal "gameOver" event bhi bhejte hain (sirf ad-hoc "wallet" message nahi) —
			// isi se client ka wahi win/lose result-screen trigger hota hai jo normal
			// (board par jeetne wali) win par hota hai, taake "opponent left" sirf ek
			// chhota toast na dikhe balke poora winner/loser screen aaye.
			room.broadcast(ServerMsg{
				Type: "events",
				Events: []Event{{
					Type:    "gameOver",
					Winner:  winner.color,
					Message: fmt.Sprintf("%s left — %s winner ban gaya", color, winner.color),
				}},
				State: room.game.Snapshot(),
			})

			newBal, err := h.store.AdjustCoins(winner.id, pot)
			if err == nil {
				winner.sendJSON(ServerMsg{Type: "wallet", Color: winner.color, Coins: newBal, Message: "opponent left — you won, pot credited"})
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

// startDisconnectGrace — net/connection drop hone par foran forfeit karne ki
// bajaye ReconnectGraceSeconds ka window deta hai. Room abhi tak isi client ki
// (dead) entry pakde rehta hai — game ki turn-timer (12s auto-play) apna kaam
// jaisa hi karti rehti hai, taake baaki players ko intezaar na karna pade.
// Agar poora grace window guzar jaye to finalizeDisconnect asal forfeiture
// (LeaveRoom) chala deta hai.
func (h *Hub) startDisconnectGrace(id string, room *Room, color Color) {
	h.mu.Lock()
	h.grace[id] = &graceEntry{room: room, color: color}
	h.mu.Unlock()

	room.broadcastExcept(color, ServerMsg{
		Type: "opponentDisconnected", Color: color, Seconds: ReconnectGraceSeconds,
		Message: "opponent's connection dropped — waiting for them to reconnect",
	})

	timer := time.AfterFunc(ReconnectGraceSeconds*time.Second, func() {
		h.finalizeDisconnect(id, room, color)
	})

	h.mu.Lock()
	if entry, ok := h.grace[id]; ok && entry.room == room {
		entry.timer = timer
	}
	h.mu.Unlock()
}

// finalizeDisconnect — grace window bina reconnect ke guzar gaya. Ab wahi
// purani forfeiture/leave logic chalti hai (pot settle, baaki players ko batana).
func (h *Hub) finalizeDisconnect(id string, room *Room, color Color) {
	h.mu.Lock()
	entry, ok := h.grace[id]
	if !ok || entry.room != room {
		h.mu.Unlock()
		return // is dauran resume ho chuka ya room hi badal chuka — purana timer hai
	}
	delete(h.grace, id)
	h.mu.Unlock()

	room.mu.Lock()
	deadClient, stillThere := room.clients[color]
	room.mu.Unlock()
	if !stillThere {
		return
	}
	h.LeaveRoom(deadClient)
}

// handleTokenLogin — app dobara khulne par (koi active game na ho) client
// apna saved auth_token bhej kar khud-b-khud login karna chahta hai. "resume"
// se yeh isliye alag hai ke "resume" sirf tab kaam karta hai jab koi
// disconnect-grace active ho (yani beech-e-game net gaya ho) — plain app-restart
// (koi game chal hi nahi raha tha) mein "resume" "koi active game nahi mili"
// keh kar reject kar deta, jis se app hamesha dobara login/signup screen par
// hi ruk jati thi chahe session valid hi kyun na ho.
func (h *Hub) handleTokenLogin(c *Client, authToken string) {
	acc, err := h.store.GetByToken(authToken)
	if err != nil {
		c.sendJSON(ServerMsg{Type: "error", Message: "session expired — please log in again"})
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
	c.sendJSON(ServerMsg{Type: "auth", PlayerID: acc.ID, AuthToken: authToken, Coins: acc.Coins, Diamonds: acc.Diamonds, Name: acc.Name, Avatar: acc.Avatar})
}

// handleResume — client "resume" message bhejta hai jab uska connection wapis
// aaye (auto ya user ke "Connect" tap karne par), sirf apna purana auth_token
// dobara bhej ke. Agar is token ke account ka koi pending disconnect-grace
// active mila to bilkul wahi room/color/game wapis mil jati hai (poori
// current Snapshot ke sath) — yeh signup/login jaisa hi pehla message hai,
// isliye ReadPump mein auth-check se pehle hi handle hota hai.
func (h *Hub) handleResume(c *Client, authToken string) {
	acc, err := h.store.GetByToken(authToken)
	if err != nil {
		c.sendJSON(ServerMsg{Type: "error", Message: "session expired — please log in again"})
		return
	}

	h.mu.Lock()
	entry, ok := h.grace[acc.ID]
	if ok {
		delete(h.grace, acc.ID)
	}
	h.mu.Unlock()
	if !ok {
		c.sendJSON(ServerMsg{Type: "error", Message: "no active game found to resume — please log in again"})
		return
	}
	if entry.timer != nil {
		entry.timer.Stop()
	}

	room := entry.room
	color := entry.color

	room.mu.Lock()
	_, stillGoing := room.clients[color]
	if stillGoing {
		room.clients[color] = c
	}
	room.mu.Unlock()
	if !stillGoing {
		// dusri taraf se game khud hi khatam ho chuki (room saaf ho chuka)
		c.sendJSON(ServerMsg{Type: "error", Message: "this game is no longer active"})
		return
	}

	c.mu.Lock()
	c.id = acc.ID
	c.name = acc.Name
	c.avatar = acc.Avatar
	c.authed = true
	c.room = room
	c.color = color
	c.mu.Unlock()

	if old := h.setActive(acc.ID, c); old != nil {
		h.kickOld(old)
	}

	profiles := map[Color]ProfileInfo{}
	room.mu.Lock()
	for col, rc := range room.clients {
		rc.mu.Lock()
		profiles[col] = ProfileInfo{Name: rc.name, Avatar: rc.avatar}
		rc.mu.Unlock()
	}
	room.mu.Unlock()

	c.sendJSON(ServerMsg{
		Type:     "resumed",
		RoomID:   room.id,
		Color:    color,
		Players:  room.game.Players,
		Mode:     room.mode,
		Bet:      room.bet,
		Coins:    acc.Coins,
		Diamonds: acc.Diamonds,
		State:    room.game.Snapshot(),
		Profiles: profiles,
	})
	room.broadcastExcept(color, ServerMsg{Type: "opponentReconnected", Color: color})
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
	room.broadcast(ServerMsg{Type: "events", Events: events, State: room.game.Snapshot(), Message: "player was inactive — server auto-played on their behalf"})
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
		winnerClient.sendJSON(ServerMsg{Type: "wallet", Color: e.Winner, Coins: newBal, Message: "you won! pot credited"})
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
		msg := "reroll's 1000-diamond limit reached for this game — locked"
		if room.game.ExtraRollTurnLocked(color) {
			msg = "is turn mein extra-roll ki 3 ki limit khatam ho chuki — agli bari par phir se mil jayegi"
		}
		c.sendJSON(ServerMsg{Type: "error", Message: msg})
		return
	}

	newDiamonds, err := h.store.AdjustDiamonds(c.id, -cost)
	if err != nil {
		c.sendJSON(ServerMsg{Type: "error", Message: "not enough diamonds for a reroll"})
		return
	}

	events, err := room.game.BuyExtraRoll(color, cost)
	if err != nil {
		// roll fail hui (jaise turn nikal chuka) — kate hue diamonds wapis kar do
		refunded, _ := h.store.AdjustDiamonds(c.id, cost)
		c.sendJSON(ServerMsg{Type: "error", Message: err.Error(), Diamonds: refunded})
		return
	}

	c.sendJSON(ServerMsg{Type: "wallet", Diamonds: newDiamonds, Message: "reroll purchased"})
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
		c.sendJSON(ServerMsg{Type: "error", Message: "email and password are both required"})
		return
	}
	if len(password) < 6 {
		c.sendJSON(ServerMsg{Type: "error", Message: "password must be at least 6 characters"})
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

// handleRequestPasswordReset — "Forgot Password" ka pehla step. Server khud
// 6-digit OTP banata hai, thodi der (5 min) memory mein rakhta hai, aur EmailJS
// ke zariye khud us email par bhej deta hai. Client ko sirf generic "bhej diya"
// jawab milta hai (yeh nahi bataya jata ke account hai ya nahi — ta ke koi email
// enumerate na kar sake).
func (h *Hub) handleRequestPasswordReset(c *Client, email string) {
	email = normalizeEmail(email)
	if email == "" {
		c.sendJSON(ServerMsg{Type: "error", Message: "email is required"})
		return
	}
	if _, err := h.store.GetIDByEmail(email); err != nil {
		// Jaan-boojh kar wahi generic success jaisa jawab — email enumeration na ho.
		c.sendJSON(ServerMsg{Type: "otpSent", Message: "if this email is registered, a verification code has been sent"})
		return
	}
	otp, err := h.otps.Issue(email)
	if err != nil {
		c.sendJSON(ServerMsg{Type: "error", Message: "could not generate code, please try again"})
		return
	}
	go func() {
		if sendErr := sendOtpEmail(email, otp); sendErr != nil {
			log.Println("sendOtpEmail failed:", sendErr)
		}
	}()
	c.sendJSON(ServerMsg{Type: "otpSent", Message: "if this email is registered, a verification code has been sent"})
}

// handleResetPassword — "Forgot Password" ka final step. Ab OTP ka asal
// match+expiry check bhi YAHIN (server par) hota hai — pehle yeh sirf client
// (app) par hota tha, jis se koi bhi seedha WebSocket message bhej kar bina
// OTP ke kisi ka bhi password badal sakta tha. Match hone par (single-use)
// code turant discard ho jata hai.
func (h *Hub) handleResetPassword(c *Client, email, newPassword, otp string) {
	email = normalizeEmail(email)
	if email == "" || newPassword == "" || otp == "" {
		c.sendJSON(ServerMsg{Type: "error", Message: "email, new password, and verification code are all required"})
		return
	}
	if len(newPassword) < 6 {
		c.sendJSON(ServerMsg{Type: "error", Message: "password must be at least 6 characters"})
		return
	}
	if !h.otps.Verify(email, otp) {
		c.sendJSON(ServerMsg{Type: "error", Message: "verification code is incorrect or expired — please request a new code"})
		return
	}
	acc, token, err := h.store.ResetPassword(email, newPassword)
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

func (h *Hub) handleUpdateProfile(c *Client, name, avatar string) {
	name = strings.TrimSpace(name)
	if name == "" {
		c.sendJSON(ServerMsg{Type: "error", Message: "name cannot be empty"})
		return
	}
	if len(name) > 16 {
		name = name[:16]
	}
	if avatar != "" {
		if strings.HasPrefix(avatar, "data:") || len(avatar) > 500 {
			c.sendJSON(ServerMsg{Type: "error", Message: "avatar must be an image URL (base64 not allowed) — first upload the photo via POST /avatar, then send the returned URL here"})
			return
		}
		if !strings.HasPrefix(avatar, "http://") && !strings.HasPrefix(avatar, "https://") {
			c.sendJSON(ServerMsg{Type: "error", Message: "avatar must be a valid http(s) URL"})
			return
		}
	}
	if err := h.store.UpdateProfile(c.id, name, avatar); err != nil {
		c.sendJSON(ServerMsg{Type: "error", Message: "could not save profile"})
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
		room := c.room
		color := c.color
		c.mu.Unlock()

		if id != "" {
			h.clearActive(id, c)
		}
		h.LeaveQueue(c)

		// Agar yeh client kisi bet wali, chal rahi game ke beech mein tha (aur
		// khud "leave" bhej kar jaan-boojh kar nahi gaya — us case mein c.room
		// pehle hi nil ho chuka hoga), to turant forfeit karne ki bajaye pehle
		// reconnect ka mauka dete hain.
		if id != "" && room != nil && room.bet > 0 && !room.game.IsOver() {
			h.startDisconnectGrace(id, room, color)
		} else {
			h.LeaveRoom(c)
		}
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
		if msg.Type == "resume" {
			h.handleResume(c, msg.AuthToken)
			continue
		}
		if msg.Type == "loginWithToken" {
			h.handleTokenLogin(c, msg.AuthToken)
			continue
		}
		if msg.Type == "requestPasswordReset" {
			h.handleRequestPasswordReset(c, msg.Email)
			continue
		}
		if msg.Type == "resetPassword" {
			h.handleResetPassword(c, msg.Email, msg.Password, msg.Otp)
			continue
		}

		c.mu.Lock()
		authed := c.authed
		c.mu.Unlock()
		if !authed {
			c.sendJSON(ServerMsg{Type: "error", Message: "please sign up or log in first"})
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
