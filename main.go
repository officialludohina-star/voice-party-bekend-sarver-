package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gorilla/websocket"
)

const maxAvatarBytes = 3 << 20 // 3MB — profile photo ki max size

// avatarUploadHandler — POST /avatar?token=<auth_token>, body = raw image
// bytes (Content-Type: image/jpeg|image/png|image/webp). Image ko file ki
// tarah disk par (AVATAR_DIR mein) save karta hai aur sirf ek hosted URL
// wapis karta hai — base64 kabhi bhi DB/JSON mein save nahi hota, sirf yeh
// URL account ki profile row mein jata hai (updateProfile WS message se).
func avatarUploadHandler(store *Store, avatarDir, publicBase string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		token := r.URL.Query().Get("token")
		acc, err := store.GetByToken(token)
		if err != nil {
			http.Error(w, "invalid token — dobara login karein", http.StatusUnauthorized)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxAvatarBytes)
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "photo bohot badi hai ya upload beech mein ruk gaya (max 3MB)", http.StatusBadRequest)
			return
		}
		if len(data) == 0 {
			http.Error(w, "khali file", http.StatusBadRequest)
			return
		}

		ext := ".jpg"
		switch r.Header.Get("Content-Type") {
		case "image/png":
			ext = ".png"
		case "image/webp":
			ext = ".webp"
		case "image/jpeg", "image/jpg":
			ext = ".jpg"
		default:
			http.Error(w, "sirf jpeg/png/webp image allowed hai (Content-Type header set karein)", http.StatusBadRequest)
			return
		}

		if err := os.MkdirAll(avatarDir, 0755); err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		filename := acc.ID + ext
		if err := os.WriteFile(filepath.Join(avatarDir, filename), data, 0644); err != nil {
			http.Error(w, "photo save nahi ho saki", http.StatusInternalServerError)
			return
		}

		base := publicBase
		if base == "" {
			base = "https://" + r.Host
		}
		url := base + "/avatars/" + filename

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"url": url})
	}
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	// Mobile app se WebView/native client aata hai, koi browser-CORS masla nahi —
	// isliye sab origins allow. Agar aage chal kar sirf apni app allow karni ho
	// to yahan Origin header check add kar sakti hain.
	CheckOrigin: func(r *http.Request) bool { return true },
}

func dbPath() string {
	if p := os.Getenv("DB_PATH"); p != "" {
		return p
	}
	return "voiceparty.db"
}

func main() {
	store, err := OpenStore(dbPath())
	if err != nil {
		log.Fatal("database open failed:", err)
	}

	hub := NewHub(store)

	// Avatar (profile photo) upload — file disk par (Railway volume mein)
	// save hota hai, sirf uska public URL wapis milta hai. AVATAR_DIR ko
	// usi volume ke andar rakhein jahan DB_PATH hai (jaise /data/avatars),
	// warna Railway restart par photos gum ho jayengi.
	avatarDir := os.Getenv("AVATAR_DIR")
	if avatarDir == "" {
		avatarDir = "avatars"
	}
	publicBase := os.Getenv("PUBLIC_BASE_URL") // e.g. https://xxxx.up.railway.app (optional — na dein to Host header se khud nikal lega)
	http.Handle("/avatars/", http.StripPrefix("/avatars/", http.FileServer(http.Dir(avatarDir))))
	http.HandleFunc("/avatar", avatarUploadHandler(store, avatarDir, publicBase))

	// Ab poora flow (signup/login + game dono) usi ek WebSocket connection ke
	// andar hota hai — koi alag HTTP request wala scene nahi. Client seedha
	// /ws se connect karta hai (token ke bagair), phir pehla message
	// {"type":"signup"} ya {"type":"login"} bhejta hai.
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			// Debug: agar upgrade fail ho to yeh exact incoming headers print karega —
			// isse pata chal jayega ke Connection/Upgrade header network mein kahin
			// strip ho raha hai ya kuch aur wajah hai.
			log.Println("upgrade error:", err, "| Connection:", r.Header.Get("Connection"), "| Upgrade:", r.Header.Get("Upgrade"), "| remote:", r.RemoteAddr, "| user-agent:", r.Header.Get("User-Agent"))
			return
		}

		client := &Client{
			conn: conn,
			send: make(chan []byte, 32),
		}

		go client.writePump()
		hub.ReadPump(client)
	})

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Println("Voice Party backend sun raha hai port par:", port)
	if err := http.ListenAndServe("0.0.0.0:"+port, nil); err != nil {
		log.Fatal(err)
	}
}
