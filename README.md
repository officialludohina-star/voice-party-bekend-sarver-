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
Har player apni **pehli extra-roll ke 2 diamonds**, phir 4, 6, 10, 16, 22, 30
— is ke baad bhi cost isi rafta se (+8 karke) thora thora badhti rehti hai:
38, 46, 54, 62... **koi doubling nahi**, sirf slow-steady growth. Ek player
apne **ek game mein total 1000 diamonds** tak hi extra-roll khareed sakta hai
(is rafta se lagbhag 18 purchases mein cap aati hai) — us ke baad lock ho
jata hai (naya game shuru hote hi dobara 2 diamonds se shuru).

**Per-turn limit:** ek hi turn ke andar (six-streak ke connected rolls samet)
player zyada se zyada **3 extra-roll** khareed sakta hai — 4th baar us turn
ke liye lock ho jata hai. Jab agli baar us player ki bari aati hai to yeh
counter phir se 0 se shuru ho jata hai (poore-game wala 1000-diamond cap
alag se, bina reset hue, chalta rehta hai).

Snapshot ke `extraRollNextCost` field mein har color ki agli cost milti hai —
`0` ka matlab hai us player ke liye abhi lock hai (isi turn ki 3-purchase
limit ki wajah se, ya poore-game 1000-diamond cap ki wajah se).

### Server → Client messages
```json
{"type": "error", "message": "..."}
{"type": "waiting", "message": "1/2 players — same bet ka koi aur player dhoond rahe hain"}
{"type": "matched", "room_id": "room-1", "color": "RED", "players": ["RED","YELLOW"], "mode": "classic", "bet": 500, "coins": 9500, "state": {...}, "profiles": {"RED": {"name":"Ali","avatar":"https://.../avatars/xxx.jpg"}, "YELLOW": {"name":"Guest","avatar":""}}}
{"type": "events", "events": [ {"type":"dice","color":"RED","value":6}, ... ], "state": {...}}
{"type": "wallet", "color": "RED", "coins": 10500, "message": "aap jeet gaye! pot credit ho gaya"}
{"type": "opponentLeft", "color": "YELLOW"}
{"type": "opponentProfile", "color": "YELLOW", "name": "Sara", "avatar": "https://.../avatars/yyy.jpg"}
{"type": "turnTimer", "color": "RED", "seconds": 12}
{"type": "forceLogout", "message": "aapki ID kisi doosre phone/device par login ho gayi hai — is device se logout kiya ja raha hai"}
```

**Bet kab katti/milti hai:** match banते hi (`matched`) har player ki bet
turant kat jati hai. Game khatam hone par (`gameOver` event) poora pot
jeetne wale ko `wallet` event ke sath mil jata hai.

## Profile (naam + display picture)
Auth response (`auth`) aur `matched` message ab har player ka `name`/`avatar`
(ya doosron ke liye `profiles` map) bhi bhejte hain — is se opponent ki
profile (naam + DP) game screen par dikhai ja sakti hai.

- Naam/avatar badalne ke liye client bhejta hai:
  `{"type":"updateProfile","name":"Ali","avatar":"https://.../avatars/xxx.jpg"}`
- **Avatar kabhi bhi base64/data-URI ke taur par nahi bheja jata** — pehle
  photo ko `POST /avatar?token=<auth_token>` par raw image bytes ke sath
  upload karein (`Content-Type: image/jpeg|image/png|image/webp`, max 3MB).
  Response: `{"url": "https://.../avatars/<id>.jpg"}` — yehi URL phir
  `updateProfile` mein bhejein. Server sirf yeh URL DB mein save karta hai,
  kabhi bhi raw image bytes/base64 nahi.
- Files disk par `AVATAR_DIR` (default `avatars`) mein save hoti hain —
  Railway par isay DB wale volume ke andar rakhein (jaise `AVATAR_DIR=/data/avatars`)
  warna restart par photos gum ho jayengi. `PUBLIC_BASE_URL` env var optional
  hai (na dein to request ke Host header se khud URL ban jata hai).

## 1 ID = 1 device (single session)
Jis waqt koi account doosri jagah se signup/login karta hai, purana
connection turant `forceLogout` message ke sath band kar diya jata hai —
ek waqt mein ek ID sirf ek hi phone/device par active reh sakti hai.

## Turn timer (inactive player handling)
Har turn (roll ya pending-move) shuru hote hi room ko `turnTimer` (12
second) bhej diya jata hai — client isi se us player ki profile par
countdown ring dikha sakta hai. Agar 12 second tak player ne roll/move
nahi kiya, server khud uski taraf se action le leta hai (auto-roll, ya
pehla legal move) taake game kabhi na atke.

## Game chhodne (leave) ya haarne par coins
- **Haarna:** bet match hote hi kat jati hai; jeetne wale ko poora pot
  `gameOver` par mil jata hai — haarne wale ke paise wapis nahi aate
  (yeh pehle se hi tha).
- **Beech mein chhod dena (leave/disconnect):** us player ko turant
  "hataa hua" treat kiya jata hai. Agar sirf ek opponent bacha (2-player
  game) to usay turant poora pot mil jata hai aur game khatam ho jati hai.
  4-player game mein sirf chhodne wale ki bet baaki bache players mein
  barabar baant di jati hai aur game jaari rehti hai (uske baad ke turns
  turn-timer khud auto-play karta rehta hai).

## Abhi scope se bahar
- 4-player games mein sirf 1st-place winner ko pura pot milta hai (leave
  ke alawa)
- Diamonds abhi sirf balance ke taur par store hote hain, koi gameplay use nahi
