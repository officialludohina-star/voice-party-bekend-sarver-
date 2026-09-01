package main

import (
	"errors"
	"math/rand"
	"sync"
)

// ==== Yeh file Ludo ka poora "authoritative" game engine hai — Kotlin client
// (LudoGameState.kt) wale rules ka hoobahoo Go port, lekin sirf abstract state
// (position 0..56 per token) par kaam karta hai. Grid coordinates/animation
// client apni taraf khud sambhalta hai; yahan sirf sach-much ka game-logic hai
// taake koi bhi player client-side cheat na kar sake.

type Color string

const (
	Red    Color = "RED"
	Green  Color = "GREEN"
	Yellow Color = "YELLOW"
	Blue   Color = "BLUE"
)

type Mode string

const (
	ModeClassic Mode = "classic"
	ModeArrow   Mode = "arrow"
	ModeQuick   Mode = "quick"
	ModeMaster  Mode = "master"
)

var ColorStart = map[Color]int{
	Green:  0,
	Yellow: 13,
	Blue:   26,
	Red:    39,
}

var PlayerColors2P = []Color{Red, Yellow}
var PlayerColors4P = []Color{Red, Green, Yellow, Blue}

var SafeSet = map[int]bool{0: true, 8: true, 13: true, 21: true, 26: true, 34: true, 39: true, 47: true}

const ArrowTailOffset = 4
const ArrowHeadOffset = 5
const ArrowEdgeEntryRel = 51
const QuickBlockRel = 46

// ==== Extra Dice Roll (diamonds se khareedi jati hai) ====
// Pehle purchase ki cost 2 diamonds, phir 4, 8, 10, 16, 24 — is ke baad har
// agli purchase pichli se double hoti jati hai. Har player apne is game mein
// total 1000 diamonds tak hi extra-roll khareed sakta hai, us ke baad lock ho
// jata hai (naya game shuru hote hi dobara se shuru ho jata hai).
var ExtraRollCosts = []int64{2, 4, 8, 10, 16, 24}

const ExtraRollGameCap int64 = 1000

func nextExtraRollCost(count int) int64 {
	if count < len(ExtraRollCosts) {
		return ExtraRollCosts[count]
	}
	cost := ExtraRollCosts[len(ExtraRollCosts)-1]
	steps := count - len(ExtraRollCosts) + 1
	for i := 0; i < steps; i++ {
		cost *= 2
	}
	return cost
}

// har color ka apna exit-arm tail cell (global ring index)
var arrowTails = func() map[int]bool {
	m := map[int]bool{}
	for _, c := range PlayerColors4P {
		m[(ColorStart[c]+ArrowTailOffset)%52] = true
	}
	return m
}()

var ArrowEdgeOwner = map[int]Color{22: Blue, 35: Red, 48: Green, 9: Yellow}

func arrowHeadFor(c Color) int {
	return (ColorStart[c] + ArrowHeadOffset) % 52
}

// ---- Events: server har action ke baad in events ko room ke saare clients ko
// broadcast karta hai; client inhi se animation/UI update karta hai. ----

type Event struct {
	Type        string           `json:"type"`
	Color       Color            `json:"color,omitempty"`
	Value       int              `json:"value,omitempty"`
	TokenIndex  int              `json:"tokenIndex,omitempty"`
	From        int              `json:"from,omitempty"`
	To          int              `json:"to,omitempty"`
	Captured    []Color          `json:"captured,omitempty"`
	ArrowJumped bool             `json:"arrowJumped,omitempty"`
	MagicBonus  bool             `json:"magicBonus,omitempty"`
	ReachedHome bool             `json:"reachedHome,omitempty"`
	Movable     []int            `json:"movable,omitempty"`
	Winner      Color            `json:"winner,omitempty"`
	FinishOrder []Color          `json:"finishOrder,omitempty"`
	RankBadge   map[Color]int    `json:"rankBadge,omitempty"`
	Message     string           `json:"message,omitempty"`
}

type GameState struct {
	mu sync.Mutex

	Mode    Mode
	Players []Color
	MagicOn bool

	Tokens      map[Color][4]int
	CurrentIdx  int
	DiceByColor map[Color]int
	SavedRolls  []int
	Movable     []int

	sixStreak    int
	chainCapture bool
	bonusSix     map[Color]bool

	FinishOrder []Color
	RankBadge   map[Color]int
	GameOver    bool
	Winner      Color

	MagicDiceCells   []int
	MagicRocketCells []int

	// Har player ne is game mein extra-roll par ab tak kitne diamonds kharch
	// kiye aur kitni dafa khareeda — 1000 diamond cap yahan se track hoti hai.
	ExtraRollSpent map[Color]int64
	ExtraRollCount map[Color]int
}

func NewGameState(mode Mode, players []Color, magicOn bool) *GameState {
	g := &GameState{
		Mode:           mode,
		Players:        players,
		MagicOn:        magicOn,
		Tokens:         map[Color][4]int{},
		DiceByColor:    map[Color]int{},
		bonusSix:       map[Color]bool{},
		RankBadge:      map[Color]int{},
		ExtraRollSpent: map[Color]int64{},
		ExtraRollCount: map[Color]int{},
	}
	for _, c := range players {
		g.Tokens[c] = [4]int{-1, -1, -1, -1}
		g.DiceByColor[c] = 1
	}
	if magicOn {
		arrowRelated := map[int]bool{}
		for c := range arrowTails {
			arrowRelated[c] = true
		}
		for _, p := range players {
			arrowRelated[arrowHeadFor(p)] = true
		}
		var pool []int
		for i := 0; i <= 51; i++ {
			if !arrowRelated[i] {
				pool = append(pool, i)
			}
		}
		rand.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
		if len(pool) >= 6 {
			g.MagicDiceCells = append([]int{}, pool[:3]...)
			g.MagicRocketCells = append([]int{}, pool[3:6]...)
		}
	}
	return g
}

func (g *GameState) CurrentColor() Color {
	return g.Players[g.CurrentIdx]
}

// IsOver — thread-safe check ke game khatam ho chuka hai ya nahi (turn-timer
// aur leave-forfeiture logic isay use karte hain).
func (g *GameState) IsOver() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.GameOver
}

// PendingAction — abhi konse player se konsa action expect ho raha hai
// (dice roll ya pending move ka intekhaab) batata hai. nil matlab game
// khatam ho chuka. Turn-timer isay use kar ke 12-second countdown arm karta
// hai aur timeout par khud hi wahi action (auto-play) le leta hai.
type PendingAction struct {
	Color   Color
	Kind    string // "roll" | "move"
	Movable []int
}

func (g *GameState) PendingAction() *PendingAction {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.GameOver {
		return nil
	}
	color := g.Players[g.CurrentIdx]
	if len(g.Movable) > 0 {
		return &PendingAction{Color: color, Kind: "move", Movable: append([]int{}, g.Movable...)}
	}
	return &PendingAction{Color: color, Kind: "roll"}
}

// ForceEnd — ek player ke game beech mein chhod jaane par (aur sirf ek hi
// player bacha ho) baaki bache hue player ko seedha winner declare kar deta
// hai, taake game turant khatam ho jaye aur pot settle ho sake.
func (g *GameState) ForceEnd(winner Color) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.GameOver {
		return
	}
	g.GameOver = true
	g.Winner = winner
}

// weightedDiceRoll: asal HTML/Kotlin jaisa hi — 6 thora zyada, 1 thora kam.
func weightedDiceRoll() int {
	weights := [6]int{10, 16, 17, 17, 16, 24}
	r := rand.Float64() * 100
	for i, w := range weights {
		if r < float64(w) {
			return i + 1
		}
		r -= float64(w)
	}
	return 6
}

// RollDice — client ke "roll" request par yeh call hota hai. Turn/pending-move
// validate karta hai, phir dice engine chalata hai aur is roll se resulting
// saare events wapis karta hai.
func (g *GameState) RollDice(requester Color) ([]Event, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.GameOver {
		return nil, errors.New("game is already over")
	}
	if requester != g.CurrentColor() {
		return nil, errors.New("not your turn")
	}
	if len(g.Movable) > 0 {
		return nil, errors.New("resolve the pending move first")
	}

	color := g.CurrentColor()
	var events []Event

	forced := g.bonusSix[color]
	val := 6
	if !forced {
		val = weightedDiceRoll()
	} else {
		g.bonusSix[color] = false
	}
	g.DiceByColor[color] = val
	events = append(events, Event{Type: "dice", Color: color, Value: val})

	if val == 6 {
		g.sixStreak++
		if g.sixStreak >= 3 {
			g.sixStreak = 0
			g.SavedRolls = nil
			g.chainCapture = false
			events = append(events, g.advanceTurn(false)...)
			return events, nil
		}
		g.SavedRolls = append(g.SavedRolls, 6)
		events = append(events, Event{Type: "rollAgain", Color: color})
		return events, nil
	}

	g.sixStreak = 0
	g.SavedRolls = append(g.SavedRolls, val)
	events = append(events, g.handleDiceResult()...)
	return events, nil
}

// NextExtraRollCost — is player ke agle extra-roll ki diamond price. Agar is
// game mein us player ki 1000 diamond ki limit khatam ho chuki ho to 0 wapis
// karta hai (matlab lock ho chuka, aur extra-roll nahi khareed sakta).
func (g *GameState) NextExtraRollCost(c Color) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	cost := nextExtraRollCost(g.ExtraRollCount[c])
	if g.ExtraRollSpent[c]+cost > ExtraRollGameCap {
		return 0
	}
	return cost
}

// BuyExtraRoll — diamonds pehle hi (hub.go mein) deduct ho chuke hote hain,
// yeh sirf spend-tracking update kar ke ek extra dice roll deta hai — bilkul
// wahi flow jo normal roll deta hai (6 par rollAgain, warna movable tokens).
func (g *GameState) BuyExtraRoll(requester Color, cost int64) ([]Event, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.GameOver {
		return nil, errors.New("game is already over")
	}
	if requester != g.CurrentColor() {
		return nil, errors.New("not your turn")
	}

	color := requester
	g.ExtraRollSpent[color] += cost
	g.ExtraRollCount[color]++

	var events []Event
	val := weightedDiceRoll()
	g.DiceByColor[color] = val
	events = append(events, Event{Type: "dice", Color: color, Value: val, Message: "extraRoll"})

	if val == 6 {
		g.sixStreak++
		if g.sixStreak >= 3 {
			g.sixStreak = 0
			g.SavedRolls = nil
			g.chainCapture = false
			events = append(events, g.advanceTurn(false)...)
			return events, nil
		}
		g.SavedRolls = append(g.SavedRolls, 6)
		events = append(events, Event{Type: "rollAgain", Color: color})
		return events, nil
	}

	g.sixStreak = 0
	g.SavedRolls = append(g.SavedRolls, val)
	events = append(events, g.handleDiceResult()...)
	return events, nil
}

func (g *GameState) computeMovable(c Color, dv int) []int {
	t := g.Tokens[c]
	var result []int
	for i, p := range t {
		if p == -1 {
			if dv == 6 {
				result = append(result, i)
			}
		} else if p+dv <= 56 {
			result = append(result, i)
		}
	}
	return result
}

func (g *GameState) legalRollsForToken(c Color, tokenIdx int) []int {
	pos := g.Tokens[c][tokenIdx]
	var legal []int
	for i, dv := range g.SavedRolls {
		if pos == -1 {
			if dv == 6 {
				legal = append(legal, i)
			}
		} else if pos+dv <= 56 {
			legal = append(legal, i)
		}
	}
	return legal
}

func distinctValues(rolls []int, idxs []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, i := range idxs {
		v := rolls[i]
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func (g *GameState) handleDiceResult() []Event {
	color := g.CurrentColor()
	var movableSet []int
	for _, dv := range g.SavedRolls {
		for _, ti := range g.computeMovable(color, dv) {
			found := false
			for _, x := range movableSet {
				if x == ti {
					found = true
					break
				}
			}
			if !found {
				movableSet = append(movableSet, ti)
			}
		}
	}
	if len(movableSet) == 0 {
		g.SavedRolls = nil
		extra := g.chainCapture
		g.chainCapture = false
		return g.advanceTurn(extra)
	}
	g.Movable = movableSet
	if len(movableSet) == 1 {
		onlyIdx := movableSet[0]
		legalIdxs := g.legalRollsForToken(color, onlyIdx)
		if len(distinctValues(g.SavedRolls, legalIdxs)) == 1 {
			return g.applyRollToToken(onlyIdx, legalIdxs[0])
		}
	}
	return []Event{{Type: "awaitMove", Color: color, Movable: movableSet}}
}

// MoveToken — client ke "move" request par call hota hai (jab awaitMove aaya ho).
// value se pata chalta hai kaunsa saved number is token par apply karna hai
// (jab ek se zyada ALAG numbers legal hon); warna pehla legal number use hota hai.
func (g *GameState) MoveToken(requester Color, tokenIdx int, value int) ([]Event, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.GameOver {
		return nil, errors.New("game is already over")
	}
	if requester != g.CurrentColor() {
		return nil, errors.New("not your turn")
	}
	found := false
	for _, m := range g.Movable {
		if m == tokenIdx {
			found = true
			break
		}
	}
	if !found {
		return nil, errors.New("that token isn't movable right now")
	}
	legalIdxs := g.legalRollsForToken(requester, tokenIdx)
	if len(legalIdxs) == 0 {
		return nil, errors.New("no legal roll for that token")
	}
	rollIdx := legalIdxs[0]
	for _, li := range legalIdxs {
		if g.SavedRolls[li] == value {
			rollIdx = li
			break
		}
	}
	return g.applyRollToToken(tokenIdx, rollIdx), nil
}

func (g *GameState) applyRollToToken(tokenIdx int, rollIdx int) []Event {
	g.Movable = nil
	dv := g.SavedRolls[rollIdx]
	g.SavedRolls = append(g.SavedRolls[:rollIdx], g.SavedRolls[rollIdx+1:]...)

	extra, moveEvents := g.performMove(tokenIdx, dv)
	events := moveEvents
	if g.GameOver {
		return events
	}
	if extra {
		g.chainCapture = true
	}
	if len(g.SavedRolls) > 0 {
		events = append(events, g.handleDiceResult()...)
	} else {
		giveExtra := g.chainCapture
		g.chainCapture = false
		events = append(events, g.advanceTurn(giveExtra)...)
	}
	return events
}

func (g *GameState) globalCellOf(c Color, pos int) int {
	return (ColorStart[c] + pos) % 52
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (g *GameState) relocateMagicCell(list *[]int, usedIdx int) {
	used := map[int]bool{}
	for _, v := range g.MagicDiceCells {
		used[v] = true
	}
	for _, v := range g.MagicRocketCells {
		used[v] = true
	}
	arrowRelated := map[int]bool{}
	for c := range arrowTails {
		arrowRelated[c] = true
	}
	for _, p := range g.Players {
		arrowRelated[arrowHeadFor(p)] = true
	}
	var pool []int
	for i := 0; i <= 51; i++ {
		if !used[i] && !arrowRelated[i] {
			pool = append(pool, i)
		}
	}
	if len(pool) == 0 {
		return
	}
	newIdx := pool[rand.Intn(len(pool))]
	for i, v := range *list {
		if v == usedIdx {
			(*list)[i] = newIdx
			break
		}
	}
}

func (g *GameState) captureAtCell(c Color, cell int) []Color {
	isArrowSpot := false
	if g.Mode == ModeArrow {
		if arrowTails[cell] {
			isArrowSpot = true
		}
		if _, ok := ArrowEdgeOwner[cell]; ok {
			isArrowSpot = true
		}
		for _, p := range g.Players {
			if arrowHeadFor(p) == cell {
				isArrowSpot = true
			}
		}
	}
	if SafeSet[cell] && !isArrowSpot {
		return nil
	}

	byColor := map[Color][]int{}
	for _, oc := range g.Players {
		if oc == c {
			continue
		}
		ot := g.Tokens[oc]
		for j, op := range ot {
			if op >= 0 && op <= 50 && g.globalCellOf(oc, op) == cell {
				byColor[oc] = append(byColor[oc], j)
			}
		}
	}
	var captured []Color
	for oc, idxs := range byColor {
		if len(idxs) >= 2 {
			continue // block — kabhi kill nahi hoti
		}
		ot := g.Tokens[oc]
		for _, j := range idxs {
			ot[j] = -1
		}
		g.Tokens[oc] = ot
		captured = append(captured, oc)
	}
	return captured
}

// performMove — ek token ko dv steps aage badhata hai aur capture/arrow/quick/
// magic rules apply karta hai. Wapis: (extraTurn, events)
func (g *GameState) performMove(tokenIdx int, dv int) (bool, []Event) {
	color := g.CurrentColor()
	t := g.Tokens[color]
	wasInYard := t[tokenIdx] == -1
	from := t[tokenIdx]
	newPos := t[tokenIdx] + dv
	if wasInYard {
		newPos = 0
	}
	if newPos > 56 {
		return false, nil
	}

	var captured []Color
	arrowJumped := false
	magicBonus := false

	if newPos <= 50 {
		if g.Mode == ModeQuick && newPos == QuickBlockRel {
			newPos = 56
		} else if g.Mode == ModeArrow {
			gc := g.globalCellOf(color, newPos)
			isTail := arrowTails[gc]
			owner, isEdge := ArrowEdgeOwner[gc]
			if isTail || (isEdge && owner == color) {
				arrowJumped = true
				if isTail {
					newPos = newPos + (ArrowHeadOffset - ArrowTailOffset)
				} else {
					newPos = ArrowEdgeEntryRel
				}
			}
		}

		if newPos <= 50 {
			gc := g.globalCellOf(color, newPos)
			if caps := g.captureAtCell(color, gc); len(caps) > 0 {
				captured = append(captured, caps...)
			}
		}

		if g.MagicOn && newPos <= 50 {
			gc := g.globalCellOf(color, newPos)
			if containsInt(g.MagicDiceCells, gc) {
				g.bonusSix[color] = true
				magicBonus = true
				g.relocateMagicCell(&g.MagicDiceCells, gc)
			} else if containsInt(g.MagicRocketCells, gc) {
				boost := rand.Intn(15) + 1
				maxAdd := boost
				if 56-newPos < maxAdd {
					maxAdd = 56 - newPos
				}
				g.relocateMagicCell(&g.MagicRocketCells, gc)
				newPos += maxAdd

				if g.Mode == ModeArrow && newPos <= 50 {
					gc2 := g.globalCellOf(color, newPos)
					isTail2 := arrowTails[gc2]
					owner2, isEdge2 := ArrowEdgeOwner[gc2]
					if isTail2 || (isEdge2 && owner2 == color) {
						arrowJumped = true
						if isTail2 {
							newPos = newPos + (ArrowHeadOffset - ArrowTailOffset)
						} else {
							newPos = ArrowEdgeEntryRel
						}
					}
				}
				if newPos <= 50 {
					gc3 := g.globalCellOf(color, newPos)
					if caps := g.captureAtCell(color, gc3); len(caps) > 0 {
						captured = append(captured, caps...)
					}
				}
			}
		}
	}

	t[tokenIdx] = newPos
	g.Tokens[color] = t
	reachedHome := newPos == 56

	events := []Event{{
		Type:        "move",
		Color:       color,
		TokenIndex:  tokenIdx,
		From:        from,
		To:          newPos,
		Captured:    captured,
		ArrowJumped: arrowJumped,
		MagicBonus:  magicBonus,
		ReachedHome: reachedHome,
	}}

	if g.checkWin(color) {
		events = append(events, Event{
			Type:        "gameOver",
			Winner:      g.Winner,
			FinishOrder: g.FinishOrder,
			RankBadge:   g.RankBadge,
		})
	}

	extra := len(captured) > 0 || arrowJumped || reachedHome
	return extra, events
}

func containsColor(list []Color, c Color) bool {
	for _, x := range list {
		if x == c {
			return true
		}
	}
	return false
}

func (g *GameState) checkWin(c Color) bool {
	t := g.Tokens[c]
	done := false
	if g.Mode == ModeQuick {
		for _, p := range t {
			if p == 56 {
				done = true
				break
			}
		}
	} else {
		done = true
		for _, p := range t {
			if p != 56 {
				done = false
				break
			}
		}
	}
	if !done {
		return false
	}

	already := containsColor(g.FinishOrder, c)
	if !already {
		g.FinishOrder = append(g.FinishOrder, c)
	}
	if !already && g.Mode == ModeQuick {
		for i, p := range t {
			if p != 56 {
				t[i] = -1
			}
		}
		g.Tokens[c] = t
	}

	if len(g.Players) == 4 {
		if !already {
			g.RankBadge[c] = len(g.FinishOrder)
		}
		if len(g.FinishOrder) >= len(g.Players)-1 {
			g.GameOver = true
			g.Winner = g.FinishOrder[0]
		}
	} else {
		g.GameOver = true
		g.Winner = c
	}
	return done
}

func (g *GameState) advanceTurn(extra bool) []Event {
	g.Movable = nil
	g.SavedRolls = nil
	g.chainCapture = false
	if g.GameOver {
		return nil
	}
	if !extra {
		next := (g.CurrentIdx + 1) % len(g.Players)
		guard := 0
		for containsColor(g.FinishOrder, g.Players[next]) && guard < len(g.Players) {
			next = (next + 1) % len(g.Players)
			guard++
		}
		g.CurrentIdx = next
		g.sixStreak = 0
	}
	return []Event{{Type: "turn", Color: g.CurrentColor()}}
}

// Snapshot — poori state ka JSON-friendly cheez, naye ya reconnect hone wale
// client ko bhejne ke liye.
type Snapshot struct {
	Mode             Mode             `json:"mode"`
	Players          []Color          `json:"players"`
	Tokens           map[Color][4]int `json:"tokens"`
	CurrentColor     Color            `json:"currentColor"`
	DiceByColor      map[Color]int    `json:"diceByColor"`
	SavedRolls       []int            `json:"savedRolls"`
	Movable          []int            `json:"movable"`
	GameOver         bool             `json:"gameOver"`
	Winner           Color            `json:"winner,omitempty"`
	FinishOrder      []Color          `json:"finishOrder,omitempty"`
	RankBadge        map[Color]int    `json:"rankBadge,omitempty"`
	MagicOn          bool             `json:"magicOn"`
	MagicDiceCells   []int            `json:"magicDiceCells,omitempty"`
	MagicRocketCells []int            `json:"magicRocketCells,omitempty"`
	ExtraRollNextCost map[Color]int64 `json:"extraRollNextCost,omitempty"`
	ExtraRollSpent    map[Color]int64 `json:"extraRollSpent,omitempty"`
}

func (g *GameState) Snapshot() *Snapshot {
	g.mu.Lock()
	defer g.mu.Unlock()

	nextCost := map[Color]int64{}
	for _, c := range g.Players {
		cost := nextExtraRollCost(g.ExtraRollCount[c])
		if g.ExtraRollSpent[c]+cost > ExtraRollGameCap {
			cost = 0 // 0 = is game mein is player ke liye lock ho chuka
		}
		nextCost[c] = cost
	}

	return &Snapshot{
		Mode:              g.Mode,
		Players:           g.Players,
		Tokens:            g.Tokens,
		CurrentColor:      g.CurrentColor(),
		DiceByColor:       g.DiceByColor,
		SavedRolls:        g.SavedRolls,
		Movable:           g.Movable,
		GameOver:          g.GameOver,
		Winner:            g.Winner,
		FinishOrder:       g.FinishOrder,
		RankBadge:         g.RankBadge,
		MagicOn:           g.MagicOn,
		MagicDiceCells:    g.MagicDiceCells,
		MagicRocketCells:  g.MagicRocketCells,
		ExtraRollNextCost: nextCost,
		ExtraRollSpent:    g.ExtraRollSpent,
	}
}
