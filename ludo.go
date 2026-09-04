package main

import (
	"errors"
	"math/rand"
	"sync"
	"time"
)

// RerollWindowSeconds — dice roll hote hi player ke paas itne seconds hote hain
// ke wo (diamonds kharch kar ke) turant dobara roll karke pehla number discard
// kar sake. Is window ke baad "buyExtraRoll" is turn ke is roll ke liye band ho
// jata hai — agla roll (agla turn ya six-streak ka agla roll) apni nayi 3-second
// window paata hai.
const RerollWindowSeconds = 3

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

// ==== Magic cells (Golden Dice + Rocket) — ab FIXED hain ====
// Pehle har naye game mein yeh 6 cells random shuffle se chuni jati thi,
// isliye har match/room mein alag jagah par hoti thin. Ab hamesha yehi
// 6 global ring-cells (0-51) use hoti hain — har game mein, har room mein
// consistent — taake players seekh sakein ke Golden Dice/Rocket hamesha
// kahan milte hain. Yeh cells jaan-boojh kar kisi bhi color ke arrow
// tail/head (4,5,17,18,30,31,43,44) se hat kar rakhi gayi hain taake
// Arrow+Magic combo mein clash na ho.
var MagicDiceCellsFixed = []int{3, 20, 37}
var MagicRocketCellsFixed = []int{12, 29, 46}

// ==== Extra Dice Roll (diamonds se khareedi jati hai) ====
// Pehle purchase ki cost 2 diamonds, phir 4, 6, 10, 16, 22, 30 — is ke baad
// bhi cost isi rafta se (aakhri do cost ka farq, yani +8) thora thora karke
// badhti rehti hai — koi doubling nahi, sirf slow-steady growth (38, 46, 54,
// 62, ...). Har player apne is game mein total 1000 diamonds tak hi
// extra-roll khareed sakta hai, us ke baad lock ho jata hai (naya game
// shuru hote hi dobara se shuru ho jata hai).
var ExtraRollCosts = []int64{2, 4, 6, 10, 16, 22, 30}

const ExtraRollGameCap int64 = 1000

// ExtraRollTurnLimit — ek hi turn (six-streak ke connected rolls samet) ke
// andar player zyada se zyada itni baar extra-roll khareed sakta hai. Is se
// aage 4th purchase us turn ke liye lock ho jata hai — jab agli baar us
// player ki bari aati hai to yeh counter phir se 0 se shuru hota hai.
const ExtraRollTurnLimit = 3

func nextExtraRollCost(count int) int64 {
	if count < len(ExtraRollCosts) {
		return ExtraRollCosts[count]
	}
	last := ExtraRollCosts[len(ExtraRollCosts)-1]
	prev := ExtraRollCosts[len(ExtraRollCosts)-2]
	step := last - prev // = 8, aakhri do defined cost ka farq
	extraSteps := int64(count - len(ExtraRollCosts) + 1)
	return last + extraSteps*step
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
	// JointTokenIndex — Master mode: agar yeh "move" event ek joint pair ka
	// tha to yahan partner token ka index milta hai (wo bhi TokenIndex ke sath
	// bilkul isi "To" position par move hua). Non-joint moves mein omit hota hai.
	JointTokenIndex *int `json:"jointTokenIndex,omitempty"`
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

	// LastRollAt — abhi tak ka sabse aakhri dice-value kab dikha (RollDice ya
	// BuyExtraRoll se) — reroll ke 3-second window ka hisaab isi se lagta hai.
	LastRollAt time.Time

	// Har player ne is game mein extra-roll par ab tak kitne diamonds kharch
	// kiye aur kitni dafa khareeda — 1000 diamond cap yahan se track hoti hai.
	ExtraRollSpent map[Color]int64
	ExtraRollCount map[Color]int

	// ExtraRollTurnCount — isi player ke CURRENT turn ke andar (six-streak ke
	// connected rolls samet) ab tak kitni baar extra-roll khareedi ja chuki hai.
	// ExtraRollTurnLimit (3) tak pohanchte hi is turn ke liye lock ho jata hai;
	// advanceTurn() naye turn ki shuruwat par isay 0 par reset kar deta hai.
	ExtraRollTurnCount map[Color]int

	// HasKilled — Quick mode ka "Kill to Enter Home" rule: is color ne is game
	// mein ab tak kam se kam ek opponent token maara hai ya nahi. Jab tak yeh
	// true na ho, is color ka koi bhi token ending track (position 51-56) mein
	// nahi ja sakta — captureAtCell mein set hota hai.
	HasKilled map[Color]bool
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
		ExtraRollSpent:     map[Color]int64{},
		ExtraRollCount:     map[Color]int{},
		ExtraRollTurnCount: map[Color]int{},
		HasKilled:          map[Color]bool{},
	}
	for _, c := range players {
		if mode == ModeQuick {
			// "Different Start" — Quick mode mein har color ka EK token game
			// shuru hote hi apne start square (relative position 0) par pehle
			// se hi hota hai, baaki teen nest (-1) mein rehte hain.
			g.Tokens[c] = [4]int{0, -1, -1, -1}
		} else {
			g.Tokens[c] = [4]int{-1, -1, -1, -1}
		}
		g.DiceByColor[c] = 1
	}
	if magicOn {
		g.MagicDiceCells = append([]int{}, MagicDiceCellsFixed...)
		g.MagicRocketCells = append([]int{}, MagicRocketCellsFixed...)
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
	g.LastRollAt = time.Now()
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
// game mein us player ki 1000 diamond ki limit khatam ho chuki ho, YA is
// current turn mein 3 extra-roll pehle hi khareed chuka ho, to 0 wapis karta
// hai (matlab abhi lock hai, aur extra-roll nahi khareed sakta).
func (g *GameState) NextExtraRollCost(c Color) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.nextExtraRollCostLocked(c)
}

func (g *GameState) nextExtraRollCostLocked(c Color) int64 {
	if g.ExtraRollTurnCount[c] >= ExtraRollTurnLimit {
		return 0
	}
	cost := nextExtraRollCost(g.ExtraRollCount[c])
	if g.ExtraRollSpent[c]+cost > ExtraRollGameCap {
		return 0
	}
	return cost
}

// ExtraRollTurnLocked — batata hai ke kya yeh player abhi sirf isi turn ki
// 3-purchase limit ki wajah se lock hai (total 1000-diamond cap alag cheez
// hai) — hub.go isay use kar ke sahi error message dikhata hai.
func (g *GameState) ExtraRollTurnLocked(c Color) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ExtraRollTurnCount[c] >= ExtraRollTurnLimit
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
	if time.Since(g.LastRollAt) > RerollWindowSeconds*time.Second {
		return nil, errors.New("reroll ka 3-second window khatam ho chuka — is roll ke liye ab dobara nahi guma saktay")
	}
	if g.ExtraRollTurnCount[requester] >= ExtraRollTurnLimit {
		return nil, errors.New("is turn mein extra-roll ki 3 ki limit khatam ho chuki — agli bari par phir se mil jayegi")
	}

	color := requester
	g.ExtraRollSpent[color] += cost
	g.ExtraRollCount[color]++
	g.ExtraRollTurnCount[color]++

	// Pichla (abhi tak apply na hua) roll discard kar dein — yeh REPLACE hai,
	// naya roll iske upar "extra" ban kar add nahi hota. Agar player ne is
	// beech mein kahin move apply kar diya ho (SavedRolls pehle hi khaali ho
	// chuka) to sirf naya roll add ho jata hai.
	if len(g.SavedRolls) > 0 {
		g.SavedRolls = g.SavedRolls[:len(g.SavedRolls)-1]
	}
	g.Movable = nil

	var events []Event
	val := weightedDiceRoll()
	g.DiceByColor[color] = val
	g.LastRollAt = time.Now()
	events = append(events, Event{Type: "dice", Color: color, Value: val, Message: "reroll"})

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

// canEnterHomeStretch — "Kill to Enter Home" rule: is color ke liye position
// 50 se aage (ending track, 51-56) mein tabhi jaya ja sakta hai jab is color ne
// is game mein kam se kam ek opponent token maara ho. Quick aur Master dono
// modes mein yeh rule lagta hai (Master ke rules screenshot mein "KILL TO
// ENTER HOME" isi ka naam hai); Classic/Arrow mein yeh apply nahi hota.
func (g *GameState) canEnterHomeStretch(c Color) bool {
	if g.Mode != ModeQuick && g.Mode != ModeMaster {
		return true
	}
	return g.HasKilled[c]
}

// ==== Master mode — "Joint Tokens" ====
// Jab kisi color ke 2 tokens ek hi (non-safe) ring cell par khare ho jayein
// to wo "joint" ban jate hain aur ek roadblock ki tarah kaam karte hain
// (opponent ka koi akela token unhein maar nahi sakta — bilkul wahi purani
// "block" wali logic jo captureAtCell mein pehle se hai). Master mode mein
// is joint jode ke apne khaas move-rules hain (neeche dekhein), aur safe
// square par pohanchte hi wo khud-b-khud separate ho jate hain — isliye
// "joint" hone ka status kahin alag se store nahi karte, balke hamesha
// current positions se hi nikalte hain (masterJointPairs).

// masterJointPairs — is color ke liye token-index -> uske "joint partner" ka
// index. Sirf Master mode mein kaam karta hai; baaki modes mein hamesha khaali
// map deta hai. Ek waqt mein sirf ek hi pair consider hota hai (do token ek
// jagah) — teen/chaar tokens ek hi cell par ikattha hona bohot rare edge case
// hai aur abhi is model mein sirf pehla pair joint mana jata hai.
func (g *GameState) masterJointPairs(c Color) map[int]int {
	pairs := map[int]int{}
	if g.Mode != ModeMaster {
		return pairs
	}
	t := g.Tokens[c]
	byPos := map[int][]int{}
	for i, p := range t {
		if p < 0 || p > 50 {
			continue // yard ya ending-track ke token kabhi joint nahi hote
		}
		if SafeSet[g.globalCellOf(c, p)] {
			continue // safe square par joint tokens turant separate ho jate hain
		}
		byPos[p] = append(byPos[p], i)
	}
	for _, idxs := range byPos {
		if len(idxs) >= 2 {
			pairs[idxs[0]] = idxs[1]
			pairs[idxs[1]] = idxs[0]
		}
	}
	return pairs
}

// jointPairsList — masterJointPairs ko [[i,j], ...] unique pairs ki list mein
// convert karta hai, Snapshot mein client ko bhejne ke liye.
func (g *GameState) jointPairsList(c Color) [][2]int {
	pairs := g.masterJointPairs(c)
	seen := map[int]bool{}
	var out [][2]int
	for i, j := range pairs {
		if seen[i] || seen[j] {
			continue
		}
		seen[i] = true
		seen[j] = true
		out = append(out, [2]int{i, j})
	}
	return out
}

func (g *GameState) computeMovable(c Color, dv int) []int {
	t := g.Tokens[c]
	joints := g.masterJointPairs(c)
	seenJointPos := map[int]bool{}
	var result []int
	for i, p := range t {
		if p == -1 {
			if dv == 6 {
				result = append(result, i)
			}
			continue
		}
		if _, isJoint := joints[i]; isJoint {
			// Master mode "Moving Joint Tokens" rule: sirf EVEN roll par move
			// hote hain, wo bhi roll ki AADHI value se. Pair ki taraf se sirf
			// ek hi movable-slot banta hai (chota index leader ke taur par).
			if seenJointPos[p] {
				continue
			}
			seenJointPos[p] = true
			if dv%2 != 0 {
				continue
			}
			newPos := p + dv/2
			if newPos > 56 {
				continue
			}
			if p <= 50 && newPos > 50 && !g.canEnterHomeStretch(c) {
				continue
			}
			result = append(result, i)
			continue
		}
		if p+dv <= 56 {
			if p <= 50 && p+dv > 50 && !g.canEnterHomeStretch(c) {
				continue // Quick/Master: bina kill kiye ending track mein nahi ja sakta
			}
			result = append(result, i)
		}
	}
	return result
}

func (g *GameState) legalRollsForToken(c Color, tokenIdx int) []int {
	pos := g.Tokens[c][tokenIdx]
	var legal []int

	if _, isJoint := g.masterJointPairs(c)[tokenIdx]; isJoint {
		// Joint pair: sirf even rolls legal hain, wo bhi aadhi value se.
		for i, dv := range g.SavedRolls {
			if dv%2 != 0 {
				continue
			}
			newPos := pos + dv/2
			if newPos > 56 {
				continue
			}
			if pos <= 50 && newPos > 50 && !g.canEnterHomeStretch(c) {
				continue
			}
			legal = append(legal, i)
		}
		return legal
	}

	for i, dv := range g.SavedRolls {
		if pos == -1 {
			if dv == 6 {
				legal = append(legal, i)
			}
		} else if pos+dv <= 56 {
			if pos <= 50 && pos+dv > 50 && !g.canEnterHomeStretch(c) {
				continue // Quick/Master: bina kill kiye ending track mein nahi ja sakta
			}
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

// captureAtCell — moverIsJoint sirf Master mode mein maayne rakhta hai: agar
// yeh move khud ek joint pair ka tha, to opponent ka 2+ tokens wala roadblock
// bhi toot (kill ho) sakta hai — "Only joint tokens can kill joint tokens"
// rule. Baaki har jagah (moverIsJoint=false, ya kisi aur mode mein) 2+ wala
// block hamesha ki tarah uncapturable rehta hai.
func (g *GameState) captureAtCell(c Color, cell int, moverIsJoint bool) []Color {
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
			if !(g.Mode == ModeMaster && moverIsJoint) {
				continue // block — sirf ek doosra joint pair hi isay tod sakta hai
			}
		}
		ot := g.Tokens[oc]
		for _, j := range idxs {
			ot[j] = -1
		}
		g.Tokens[oc] = ot
		captured = append(captured, oc)
	}
	if len(captured) > 0 {
		// Quick mode ka "Kill to Enter Home" rule — is color ne ab kam se kam
		// ek kill kar li, ab is ke tokens ending track mein ja sakte hain.
		g.HasKilled[c] = true
	}
	return captured
}

// performMove — ek token ko dv steps aage badhata hai aur capture/arrow/quick/
// magic rules apply karta hai. Wapis: (extraTurn, events)
func (g *GameState) performMove(tokenIdx int, dv int) (bool, []Event) {
	color := g.CurrentColor()
	t := g.Tokens[color]

	// Master mode: agar yeh token abhi kisi doosre token ke sath "joint" hai
	// to move dv ki AADHI value se hota hai aur partner token bhi sath move
	// hota hai (dono ek sath, ek hi naye position par).
	partnerIdx, isJointMove := g.masterJointPairs(color)[tokenIdx]
	moveAmount := dv
	if isJointMove {
		moveAmount = dv / 2
	}

	wasInYard := t[tokenIdx] == -1
	from := t[tokenIdx]
	newPos := t[tokenIdx] + moveAmount
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
			if caps := g.captureAtCell(color, gc, isJointMove); len(caps) > 0 {
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
				// Rocket rule: "no more than 8 squares" — random 1-8 (pehle
				// galti se 1-15 tha).
				boost := rand.Intn(8) + 1
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
					if caps := g.captureAtCell(color, gc3, isJointMove); len(caps) > 0 {
						captured = append(captured, caps...)
					}
				}
			}
		}
	}

	t[tokenIdx] = newPos
	if isJointMove {
		// Joint pair — partner token bhi bilkul isi naye position par jata hai.
		t[partnerIdx] = newPos
	}
	g.Tokens[color] = t
	reachedHome := newPos == 56

	moveEvent := Event{
		Type:        "move",
		Color:       color,
		TokenIndex:  tokenIdx,
		From:        from,
		To:          newPos,
		Captured:    captured,
		ArrowJumped: arrowJumped,
		MagicBonus:  magicBonus,
		ReachedHome: reachedHome,
	}
	if isJointMove {
		p := partnerIdx
		moveEvent.JointTokenIndex = &p
	}
	events := []Event{moveEvent}

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
		// Naye player ki bari shuru — is player ka extra-roll turn-counter
		// (pichli baar jab is ki bari thi) reset kar dein taake usay phir se
		// 3 fresh extra-roll milein.
		g.ExtraRollTurnCount[g.CurrentColor()] = 0
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
	// JointTokens — Master mode: har color ke abhi ke joint pairs (token-index
	// jode ki list), taake client bina khud hisaab lagaye unhein ek roadblock
	// ki tarah dikha sake. Baaki modes mein hamesha khaali/omit rehta hai.
	JointTokens map[Color][][2]int `json:"jointTokens,omitempty"`
}

func (g *GameState) Snapshot() *Snapshot {
	g.mu.Lock()
	defer g.mu.Unlock()

	nextCost := map[Color]int64{}
	for _, c := range g.Players {
		// 0 = abhi is player ke liye lock hai — ya to isi turn ki 3-purchase
		// limit ki wajah se, ya poore game ke 1000-diamond cap ki wajah se.
		nextCost[c] = g.nextExtraRollCostLocked(c)
	}

	var jointTokens map[Color][][2]int
	if g.Mode == ModeMaster {
		jointTokens = map[Color][][2]int{}
		for _, c := range g.Players {
			if pairs := g.jointPairsList(c); len(pairs) > 0 {
				jointTokens[c] = pairs
			}
		}
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
		JointTokens:       jointTokens,
	}
}
