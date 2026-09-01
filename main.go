package main

import (
	"log"
	"net/http"
	"os"

	"github.com/gorilla/websocket"
)

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
