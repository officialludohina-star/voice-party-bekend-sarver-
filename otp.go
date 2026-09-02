package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// ============================================================================
// Forgot-password OTP — ab poori tarah SERVER par generate aur verify hoti hai.
// Pehle app khud OTP bana kar khud hi verify kar leta tha aur seedha
// resetPassword call kar deta tha — jiska matlab tha koi bhi (bina kisi ka
// email inbox dekhe) kisi doosre ke email + koi bhi naya password bhej kar
// account hijack kar sakta tha. Ab flow yeh hai:
//   1) client "requestPasswordReset" {email} bhejta hai
//   2) server khud 6-digit code banata hai, memory mein (5 min expiry ke
//      sath) rakhta hai, aur EmailJS ke zariye khud bhi email bhej deta hai
//   3) client "resetPassword" {email, password, otp} bhejta hai — server
//      code match + expiry check karne ke baad hi password badalta hai
// ============================================================================

const otpTTL = 5 * time.Minute

// EmailJS credentials — Kotlin client (EmailService.kt) mein bhi yehi
// service/template/public-key use hote hain (EmailJS "public key" client aur
// server dono se call karne ke liye hi banayi jati hai).
const (
	emailjsServiceID  = "service_1d2y28h"
	emailjsTemplateID = "template_diw9ywh"
	emailjsPublicKey  = "1mgEysCMhFFaok_Fb"
	emailjsURL        = "https://api.emailjs.com/api/v1.0/email/send"
)

type otpEntry struct {
	code    string
	expires time.Time
}

// OtpStore — thread-safe, sirf memory mein (disk/DB par save karne ki zaroorat
// nahi, chand minute ke liye hi zinda rehta hai).
type OtpStore struct {
	mu      sync.Mutex
	entries map[string]otpEntry
}

func NewOtpStore() *OtpStore {
	return &OtpStore{entries: map[string]otpEntry{}}
}

// Issue — naya 6-digit code banata hai, store karta hai, aur wapis karta hai
// (caller isi ko email kare ga).
func (s *OtpStore) Issue(email string) (string, error) {
	code, err := generateNumericOtp(6)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.entries[email] = otpEntry{code: code, expires: time.Now().Add(otpTTL)}
	s.mu.Unlock()
	return code, nil
}

// Verify — code match aur expiry check karta hai. Kaamyab verify hone par
// (single-use) entry turant hata di jati hai taake dobara wahi code kaam na kare.
func (s *OtpStore) Verify(email, code string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[email]
	if !ok {
		return false
	}
	if time.Now().After(entry.expires) || entry.code != code {
		return false
	}
	delete(s.entries, email)
	return true
}

func generateNumericOtp(digits int) (string, error) {
	max := big.NewInt(1)
	for i := 0; i < digits; i++ {
		max.Mul(max, big.NewInt(10))
	}
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", digits, n.Int64()), nil
}

// sendOtpEmail — EmailJS REST API ke zariye khud server se OTP email bhejta
// hai (Kotlin app ke EmailService.kt jaisa hi request-shape).
func sendOtpEmail(toEmail, otp string) error {
	payload := map[string]interface{}{
		"service_id": emailjsServiceID,
		"template_id": emailjsTemplateID,
		"user_id":     emailjsPublicKey,
		"template_params": map[string]string{
			"to_email":  toEmail,
			"email":     toEmail,
			"name":      "Voice Party Ludo",
			"from_name": "Voice Party Ludo",
			"otp_code":  otp,
			"message":   otp,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, emailjsURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("emailjs HTTP %d", resp.StatusCode)
	}
	return nil
}
