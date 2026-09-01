package main

import (
	"encoding/json"
	"net/http"
)

// ==== /signup aur /login — plain JSON HTTP endpoints (WebSocket se pehle
// yehi use hote hain). Dono account ka "token" wapis dete hain, jo phir /ws
// connect karte waqt query param ke taur par bhejna hota hai. ====

type authRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type authResponse struct {
	PlayerID string `json:"player_id,omitempty"`
	Token    string `json:"token,omitempty"`
	Coins    int64  `json:"coins,omitempty"`
	Diamonds int64  `json:"diamonds,omitempty"`
	Message  string `json:"message,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// signupHandler — naya account banata hai: 10,000 coins + 30 diamonds signup
// bonus ke sath. Password kabhi plain-text save nahi hota (bcrypt hash).
func signupHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, authResponse{Message: "POST method use karein"})
			return
		}
		var req authRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Email == "" || req.Password == "" {
			writeJSON(w, http.StatusBadRequest, authResponse{Message: "email aur password dono zaroori hain"})
			return
		}
		if len(req.Password) < 6 {
			writeJSON(w, http.StatusBadRequest, authResponse{Message: "password kam se kam 6 characters ka ho"})
			return
		}
		acc, token, err := store.SignUp(req.Email, req.Password)
		if err != nil {
			writeJSON(w, http.StatusConflict, authResponse{Message: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, authResponse{PlayerID: acc.ID, Token: token, Coins: acc.Coins, Diamonds: acc.Diamonds})
	}
}

func loginHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, authResponse{Message: "POST method use karein"})
			return
		}
		var req authRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, authResponse{Message: "bad request"})
			return
		}
		acc, token, err := store.Login(req.Email, req.Password)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, authResponse{Message: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, authResponse{PlayerID: acc.ID, Token: token, Coins: acc.Coins, Diamonds: acc.Diamonds})
	}
}
