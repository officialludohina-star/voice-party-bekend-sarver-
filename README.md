# Voice Party — Go Backend (Pure WebSocket)

Ab poora flow — **signup, login, aur game sab kuch ek hi WebSocket connection**
ke andar hota hai. Koi alag HTTP request/response wala scene nahi hai; sirf
`/ws` par connect karo aur uske baad sab kuch JSON messages se hi hota hai.

- **Accounts** — signup/login (WS message se), password bcrypt-hashed, naya
  account banate hi **10,000 coins + 30 diamonds** signup bonus
- **Matchmaking** — same bet amount wale real online players ko match karta
  hai (koi timeout nahi — match na milay to player waiting mein hi rehta hai)
- **Authoritative Ludo engine** — dice roll, token movement, capture,
  arrow/quick/master/magic mode — sab rules server par chalte hain; jeetne
  par poora pot winner ke account mein automatically credit ho jata hai

## Files
- `main.go` — HTTP server sirf `/ws` (websocket) aur `/health` ke liye
- `db.go` — SQLite account store (signup bonus, password hash, coin wallet)
- `hub.go` — client connections, signup/login (WS message), matchmaking
  queue, rooms, bet escrow, pot settlement
- `ludo.go` — poora game engine (rules)
- `Dockerfile` + `railway.json` — Railway deployment

## Local run (agar Go installed ho)
```
go mod tidy
go run .
```
Server `PORT` env var use karta hai (default 8080). Database file `voiceparty.db`.

## Railway par deploy
1. Is folder ko GitHub repo bana kar push kar dein.
2. Railway dashboard mein "New Project" → "Deploy from GitHub repo".
3. Railway khud `Dockerfile` detect kar ke build kar lega.
4. **Zaroori:** Railway ka filesystem restart par khali ho jata hai — apni
   service par "Volumes" se ek Volume add karein (jaise `/data`), phir
   `DB_PATH=/data/voiceparty.db` environment variable set karein.
5. Deploy hone ke baad WebSocket URL: `wss://xxxx.up.railway.app/ws`

## WebSocket protocol

Connect: `wss://<your-app>.up.railway.app/ws` (koi query param/token zaroori nahi)

### Step 1 — Signup ya Login (connect hote hi sabse pehla message)
```json
{"type": "signup", "email": "user@gmail.com", "password": "kam se kam 6 characters"}
```
ya
```json
{"type": "login", "email": "user@gmail.com", "password": "..."}
```

Server jawab deta hai:
```json
{"type": "auth", "player_id": "a1b2c3...", "auth_token": "xxxx...", "coins": 10000, "diamonds": 30}
```
`auth_token` app mein save kar lein (jaise EncryptedSharedPreferences mein) —
agli baar reconnect par bhi login/signup message hi bhejna hoga (session ek
socket connection tak hi rehta hai).

### Step 2 — Auth hone ke baad game messages
```json
{"type": "join", "bet": 500, "mode": "classic", "players": 2, "magic": false}
{"type": "roll"}
{"type": "move", "token": 0, "value": 6}
{"type": "buyExtraRoll"}
{"type": "leave"}
```
- `mode`: "classic" | "arrow" | "quick" | "master"
- `players`: 2 ya 4
- `move.token`: token-INDEX (0-3)
- `move.value`: sirf tab zaroori jab `awaitMove` event mein ek se zyada legal
  numbers hon (warna 0 bhej dein)
- `buyExtraRoll`: koi field nahi chahiye — sirf current-turn player hi bhej
  sakta hai, cost server khud nikal leta hai

### Extra Dice Roll (diamonds se)
Har player apni **pehli extra-roll ke 2 diamonds**, phir 4, 8, 10, 16, 24 —
is ke baad har agli purchase pichli se **double** hoti jati hai. Ek player
apne **ek game mein total 1000 diamonds** tak hi extra-roll khareed sakta hai
— us ke baad lock ho jata hai (naya game shuru hote hi dobara 2 diamonds se
shuru). Snapshot ke `extraRollNextCost` field mein har color ki agli cost
milti hai — `0` ka matlab hai us player ke liye is game mein lock ho chuka.

### Server → Client messages
```json
{"type": "error", "message": "..."}
{"type": "waiting", "message": "1/2 players — same bet ka koi aur player dhoond rahe hain"}
{"type": "matched", "room_id": "room-1", "color": "RED", "players": ["RED","YELLOW"], "mode": "classic", "bet": 500, "coins": 9500, "state": {...}}
{"type": "events", "events": [ {"type":"dice","color":"RED","value":6}, ... ], "state": {...}}
{"type": "wallet", "color": "RED", "coins": 10500, "message": "aap jeet gaye! pot credit ho gaya"}
{"type": "opponentLeft", "color": "YELLOW"}
```

**Bet kab katti/milti hai:** match banते hi (`matched`) har player ki bet
turant kat jati hai. Game khatam hone par (`gameOver` event) poora pot
jeetne wale ko `wallet` event ke sath mil jata hai.

## Abhi scope se bahar
- Disconnect hone par sirf `opponentLeft` batataya jata hai — bet wapis
  nahi hoti, auto-forfeit/reconnect-grace-period abhi nahi hai
- 4-player games mein sirf 1st-place winner ko pura pot milta hai
- Diamonds abhi sirf balance ke taur par store hote hain, koi gameplay use nahi
