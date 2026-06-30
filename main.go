package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha512"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

// --- Data Models ---

type Product struct {
	ID        int
	Name      string
	Price     float64
	Category  string
	Available bool
	ImageURL  string
}

type CartItem struct {
	Product  Product
	Quantity int
}

type Session struct {
	State            string
	Cart             []CartItem
	Pickup           string
	DeliveryType     string
	DeliveryAddress  string
	BrowsingCategory string
	PendingOrderID   string
}

// --- Globals ---

var sessions = map[string]*Session{}

const DeliveryFee = 500.0

var pickupSlots = []string{
	"10:00 AM - 11:00 AM",
	"12:00 PM - 1:00 PM",
	"3:00 PM - 4:00 PM",
	"5:00 PM - 6:00 PM",
}

var db *sql.DB

// --- Database setup ---

func initDB() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL environment variable is not set")
	}

	var err error
	db, err = sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatal("Failed to open database:", err)
	}

	if err = db.Ping(); err != nil {
		log.Fatal("Failed to connect to database:", err)
	}

	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS orders (
		id SERIAL PRIMARY KEY,
		order_id TEXT NOT NULL,
		phone TEXT NOT NULL,
		items TEXT NOT NULL,
		total REAL NOT NULL,
		delivery_type TEXT DEFAULT 'pickup',
		delivery_address TEXT DEFAULT '',
		pickup_slot TEXT DEFAULT '',
		status TEXT DEFAULT 'pending_payment',
		payment_status TEXT DEFAULT 'unpaid',
		paystack_ref TEXT DEFAULT '',
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);`)
	if err != nil {
		log.Fatal("Failed to create orders table:", err)
	}

	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS products (
		id SERIAL PRIMARY KEY,
		name TEXT NOT NULL,
		price REAL NOT NULL,
		category TEXT DEFAULT 'General',
		available BOOLEAN DEFAULT true
	);`)
	if err != nil {
		log.Fatal("Failed to create products table:", err)
	}

	db.Exec(`ALTER TABLE orders ADD COLUMN IF NOT EXISTS delivery_type TEXT DEFAULT 'pickup'`)
	db.Exec(`ALTER TABLE orders ADD COLUMN IF NOT EXISTS delivery_address TEXT DEFAULT ''`)
	db.Exec(`ALTER TABLE orders ADD COLUMN IF NOT EXISTS payment_status TEXT DEFAULT 'unpaid'`)
	db.Exec(`ALTER TABLE orders ADD COLUMN IF NOT EXISTS paystack_ref TEXT DEFAULT ''`)
	db.Exec(`ALTER TABLE products ADD COLUMN IF NOT EXISTS category TEXT DEFAULT 'General'`)
	db.Exec(`ALTER TABLE products ADD COLUMN IF NOT EXISTS image_url TEXT DEFAULT ''`)
	db.Exec(`UPDATE orders SET payment_status = 'paid', status = 'pending' WHERE payment_status IS NULL OR payment_status = ''`)

	log.Println("✅ Database ready")
}

// --- Order ID generator ---

func generateOrderID() string {
	chars := "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	id := make([]byte, 4)
	for i := range id {
		id[i] = chars[rand.Intn(len(chars))]
	}
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM orders WHERE order_id = $1`, string(id)).Scan(&count)
	if count > 0 {
		return generateOrderID()
	}
	return string(id)
}

// --- Business hours ---

func isOpen() bool {
	openingHour, _ := strconv.Atoi(os.Getenv("OPENING_HOUR"))
	closingHour, _ := strconv.Atoi(os.Getenv("CLOSING_HOUR"))
	if openingHour == 0 && closingHour == 0 {
		return true
	}
	hour := time.Now().Hour()
	return hour >= openingHour && hour < closingHour
}

func closedMessage() string {
	openingHour, _ := strconv.Atoi(os.Getenv("OPENING_HOUR"))
	shopName := os.Getenv("SHOP_NAME")
	if shopName == "" {
		shopName = "we"
	}
	return fmt.Sprintf("🔒 *%s are currently closed.*\n\nWe open at *%d:00 AM* daily.\n\nPlease send *hi* when we're open to place your order. Thank you! 😊", shopName, openingHour)
}

// --- Product helpers ---

func getProducts() ([]Product, error) {
	rows, err := db.Query(`SELECT id, name, price, category, available, image_url FROM products WHERE available = true ORDER BY category, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var products []Product
	for rows.Next() {
		var p Product
		rows.Scan(&p.ID, &p.Name, &p.Price, &p.Category, &p.Available, &p.ImageURL)
		products = append(products, p)
	}
	return products, nil
}

func getAllProducts() ([]Product, error) {
	rows, err := db.Query(`SELECT id, name, price, category, available, image_url FROM products ORDER BY category, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var products []Product
	for rows.Next() {
		var p Product
		rows.Scan(&p.ID, &p.Name, &p.Price, &p.Category, &p.Available, &p.ImageURL)
		products = append(products, p)
	}
	return products, nil
}

func getCategories() ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT category FROM products WHERE available = true ORDER BY category`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var categories []string
	for rows.Next() {
		var c string
		rows.Scan(&c)
		categories = append(categories, c)
	}
	return categories, nil
}

func getProductsByCategory(category string) ([]Product, error) {
	rows, err := db.Query(`SELECT id, name, price, category, available, image_url FROM products WHERE available = true AND category = $1 ORDER BY id`, category)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var products []Product
	for rows.Next() {
		var p Product
		rows.Scan(&p.ID, &p.Name, &p.Price, &p.Category, &p.Available, &p.ImageURL)
		products = append(products, p)
	}
	return products, nil
}

func isReturningCustomer(phone string) bool {
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM orders WHERE phone = $1 AND payment_status = 'paid'`, phone).Scan(&count)
	return count > 0
}

// --- Order helpers ---

func saveOrder(phone string, session *Session, total float64) (string, error) {
	orderID := generateOrderID()
	var itemParts []string
	for _, item := range session.Cart {
		itemParts = append(itemParts, fmt.Sprintf("%s x%d", item.Product.Name, item.Quantity))
	}
	itemsStr := strings.Join(itemParts, ", ")
	_, err := db.Exec(
		`INSERT INTO orders (order_id, phone, items, total, delivery_type, delivery_address, pickup_slot, status, payment_status) VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending_payment', 'unpaid')`,
		orderID, phone, itemsStr, total, session.DeliveryType, session.DeliveryAddress, session.Pickup,
	)
	if err != nil {
		return "", err
	}
	return orderID, nil
}

func getOrdersByPhone(phone string) ([]string, error) {
	rows, err := db.Query(
		`SELECT order_id, items, total, delivery_type, pickup_slot, status, payment_status, created_at FROM orders WHERE phone = $1 ORDER BY created_at DESC LIMIT 5`,
		phone,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var orderID, items, deliveryType, pickupSlot, status, paymentStatus, createdAt string
		var total float64
		rows.Scan(&orderID, &items, &total, &deliveryType, &pickupSlot, &status, &paymentStatus, &createdAt)
		delivery := pickupSlot
		if deliveryType == "delivery" {
			delivery = "Delivery"
		}
		payIcon := "💳 unpaid"
		if paymentStatus == "paid" {
			payIcon = "✅ paid"
		}
		lines = append(lines, fmt.Sprintf("• *%s* — %s\n  NGN %.0f | %s | %s | _%s_", orderID, items, total, delivery, payIcon, status))
	}
	return lines, nil
}

func getOrderStatus(orderID string) (string, string, string, string, float64, error) {
	var phone, items, status, paymentStatus string
	var total float64
	err := db.QueryRow(
		`SELECT phone, items, status, payment_status, total FROM orders WHERE order_id = $1`,
		strings.ToUpper(orderID),
	).Scan(&phone, &items, &status, &paymentStatus, &total)
	return phone, items, status, paymentStatus, total, err
}

// --- Message sending ---

// Detect which provider to use based on env vars
func useMetaAPI() bool {
	return os.Getenv("META_ACCESS_TOKEN") != "" && os.Getenv("META_PHONE_NUMBER_ID") != ""
}

func sendWhatsAppMessage(to, body string) error {
	if useMetaAPI() {
		return sendMetaMessage(to, body)
	}
	return sendTwilioMessage(to, body)
}

func sendWhatsAppImage(to, imageURL, caption string) error {
	if useMetaAPI() {
		return sendMetaImage(to, imageURL, caption)
	}
	return sendTwilioImage(to, imageURL, caption)
}

func sendTwilioImage(to, imageURL, caption string) error {
	accountSID := os.Getenv("TWILIO_ACCOUNT_SID")
	authToken := os.Getenv("TWILIO_AUTH_TOKEN")
	fromNumber := os.Getenv("TWILIO_WHATSAPP_NUMBER")

	apiURL := fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%s/Messages.json", accountSID)
	msgData := url.Values{}
	msgData.Set("To", "whatsapp:"+to)
	msgData.Set("From", fromNumber)
	msgData.Set("Body", caption)
	msgData.Set("MediaUrl", imageURL)

	client := &http.Client{}
	req, err := http.NewRequest("POST", apiURL, strings.NewReader(msgData.Encode()))
	if err != nil {
		return err
	}
	req.SetBasicAuth(accountSID, authToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("twilio error: %s", resp.Status)
	}
	return nil
}

func sendMetaImage(to, imageURL, caption string) error {
	accessToken := os.Getenv("META_ACCESS_TOKEN")
	phoneNumberID := os.Getenv("META_PHONE_NUMBER_ID")

	apiURL := fmt.Sprintf("https://graph.facebook.com/v18.0/%s/messages", phoneNumberID)

	payload := map[string]interface{}{
		"messaging_product": "whatsapp",
		"to":                to,
		"type":              "image",
		"image": map[string]string{
			"link":    imageURL,
			"caption": caption,
		},
	}

	payloadBytes, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", apiURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("meta error: %s - %s", resp.Status, string(body))
	}
	return nil
}

func sendTwilioMessage(to, body string) error {
	accountSID := os.Getenv("TWILIO_ACCOUNT_SID")
	authToken := os.Getenv("TWILIO_AUTH_TOKEN")
	fromNumber := os.Getenv("TWILIO_WHATSAPP_NUMBER")

	apiURL := fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%s/Messages.json", accountSID)
	msgData := url.Values{}
	msgData.Set("To", "whatsapp:"+to)
	msgData.Set("From", fromNumber)
	msgData.Set("Body", body)

	client := &http.Client{}
	req, err := http.NewRequest("POST", apiURL, strings.NewReader(msgData.Encode()))
	if err != nil {
		return err
	}
	req.SetBasicAuth(accountSID, authToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("twilio error: %s", resp.Status)
	}
	return nil
}

func sendMetaMessage(to, body string) error {
	accessToken := os.Getenv("META_ACCESS_TOKEN")
	phoneNumberID := os.Getenv("META_PHONE_NUMBER_ID")

	apiURL := fmt.Sprintf("https://graph.facebook.com/v18.0/%s/messages", phoneNumberID)

	payload := map[string]interface{}{
		"messaging_product": "whatsapp",
		"to":                to,
		"type":              "text",
		"text":              map[string]string{"body": body},
	}

	payloadBytes, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", apiURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("meta error: %s - %s", resp.Status, string(body))
	}
	return nil
}

// --- Notification helpers ---

func notifyAdmin(orderID, phone, items string, total float64, session *Session) {
	shopName := os.Getenv("SHOP_NAME")
	if shopName == "" {
		shopName = "Supermarket"
	}
	deliveryInfo := fmt.Sprintf("Pickup: %s", session.Pickup)
	if session.DeliveryType == "delivery" {
		deliveryInfo = fmt.Sprintf("Delivery to: %s", session.DeliveryAddress)
	}
	msg := fmt.Sprintf(
		"🔔 *New Paid Order - %s*\n\nOrder ID: *%s*\nCustomer: %s\nItems: %s\nTotal: NGN %.0f\n%s\n\nUpdate status at: %s/admin/orders",
		shopName, orderID, phone, items, total, deliveryInfo,
		os.Getenv("RAILWAY_PUBLIC_DOMAIN"),
	)
	adminPhones := strings.Split(os.Getenv("ADMIN_PHONE"), ",")
	for _, adminPhone := range adminPhones {
		adminPhone = strings.TrimSpace(adminPhone)
		if adminPhone != "" {
			if err := sendWhatsAppMessage(adminPhone, msg); err != nil {
				log.Printf("Failed to notify admin %s: %v", adminPhone, err)
			}
		}
	}
}

func notifyCustomer(phone, orderID, status string) {
	shopName := os.Getenv("SHOP_NAME")
	if shopName == "" {
		shopName = "Supermarket"
	}
	var msg string
	switch status {
	case "ready":
		msg = fmt.Sprintf("✅ *Your order is ready!*\n\nHi! Your order *#%s* at %s is ready for pickup.\n\nPlease come collect your items. Thank you! 🛒", orderID, shopName)
	case "out_for_delivery":
		msg = fmt.Sprintf("🚚 *Your order is on the way!*\n\nYour order *#%s* from %s is out for delivery.\n\nEstimated arrival: *30 minutes* ⏱️\n\nPlease be available to receive your items!", orderID, shopName)
	case "completed":
		msg = fmt.Sprintf("🎉 *Order Completed!*\n\nYour order *#%s* has been marked as completed.\n\nThank you for shopping at %s! We hope to see you again soon. 😊", orderID, shopName)
	}
	if msg != "" {
		if err := sendWhatsAppMessage(phone, msg); err != nil {
			log.Printf("Failed to notify customer: %v", err)
		}
	}
}

// --- Paystack helpers ---

// --- Cloudinary helpers ---

func sha1Hex(s string) string {
	h := sha1.New()
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}

func uploadToCloudinary(file multipart.File, filename string) (string, error) {
	cloudName := os.Getenv("CLOUDINARY_CLOUD_NAME")
	apiKey := os.Getenv("CLOUDINARY_API_KEY")
	apiSecret := os.Getenv("CLOUDINARY_API_SECRET")

	timestamp := fmt.Sprintf("%d", time.Now().Unix())

	// Cloudinary signed uploads use SHA-1 over the params-to-sign string + api secret
	signString := fmt.Sprintf("timestamp=%s%s", timestamp, apiSecret)
	signature := sha1Hex(signString)

	var b bytes.Buffer
	writer := multipart.NewWriter(&b)

	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, file); err != nil {
		return "", err
	}

	writer.WriteField("timestamp", timestamp)
	writer.WriteField("api_key", apiKey)
	writer.WriteField("signature", signature)
	writer.Close()

	uploadURL := fmt.Sprintf("https://api.cloudinary.com/v1_1/%s/image/upload", cloudName)
	req, err := http.NewRequest("POST", uploadURL, &b)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		SecureURL string `json:"secure_url"`
		Error     struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if result.SecureURL == "" {
		return "", fmt.Errorf("cloudinary upload failed: %s", result.Error.Message)
	}
	return result.SecureURL, nil
}

func createPaystackPaymentLink(orderID, phone string, totalKobo int) (string, error) {
	secretKey := os.Getenv("PAYSTACK_SECRET_KEY")
	payload := map[string]interface{}{
		"email":     phone + "@bot.com",
		"amount":    totalKobo,
		"reference": "ORDER-" + orderID + "-" + fmt.Sprintf("%d", time.Now().UnixMilli()),
		"metadata": map[string]string{
			"order_id": orderID,
			"phone":    phone,
		},
		"callback_url": os.Getenv("RAILWAY_PUBLIC_DOMAIN") + "/paystack/callback",
	}
	payloadBytes, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", "https://api.paystack.co/transaction/initialize", strings.NewReader(string(payloadBytes)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		Status bool `json:"status"`
		Data   struct {
			AuthorizationURL string `json:"authorization_url"`
			Reference        string `json:"reference"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if !result.Status {
		return "", fmt.Errorf("paystack error")
	}
	db.Exec(`UPDATE orders SET paystack_ref = $1 WHERE order_id = $2`, result.Data.Reference, orderID)
	return result.Data.AuthorizationURL, nil
}

// --- Auth middleware ---

func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("admin_session")
		if err == nil && cookie.Value == os.Getenv("ADMIN_PASSWORD") {
			next(w, r)
			return
		}
		if r.Method == http.MethodPost {
			r.ParseForm()
			password := r.FormValue("password")
			if password == os.Getenv("ADMIN_PASSWORD") {
				http.SetCookie(w, &http.Cookie{
					Name:     "admin_session",
					Value:    password,
					Path:     "/",
					MaxAge:   86400 * 7,
					HttpOnly: true,
				})
				http.Redirect(w, r, r.URL.Path, http.StatusSeeOther)
				return
			}
			showLoginPage(w, "❌ Wrong password. Try again.")
			return
		}
		showLoginPage(w, "")
	}
}

func showLoginPage(w http.ResponseWriter, errMsg string) {
	shopName := os.Getenv("SHOP_NAME")
	if shopName == "" {
		shopName = "Supermarket"
	}
	errHTML := ""
	if errMsg != "" {
		errHTML = fmt.Sprintf(`<div class="error">%s</div>`, errMsg)
	}
	logoLetter := strings.ToUpper(string([]rune(shopName)[0]))
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head>
	<title>%s Admin Login</title>
	<meta name="viewport" content="width=device-width, initial-scale=1.0">
	<style>
		* { box-sizing: border-box; margin: 0; padding: 0; }
		body { font-family: Arial, sans-serif; background: #f5f5f5; display: flex; justify-content: center; align-items: center; min-height: 100vh; }
		.login-card { background: white; border-radius: 12px; padding: 32px; width: 100%%; max-width: 380px; margin: 20px; box-shadow: 0 2px 10px rgba(0,0,0,0.1); }
		.logo { width: 56px; height: 56px; margin: 0 auto 14px; background: #5c0e1e; color: #fff; border-radius: 50%%; display: flex; align-items: center; justify-content: center; font-size: 24px; font-weight: 800; }
		h1 { text-align: center; color: #333; font-size: 20px; margin-bottom: 6px; }
		p { text-align: center; color: #888; font-size: 14px; margin-bottom: 24px; }
		input[type=password] { width: 100%%; padding: 12px; border: 1px solid #ddd; border-radius: 8px; font-size: 15px; margin-bottom: 14px; }
		button { width: 100%%; padding: 12px; background: #5c0e1e; color: white; border: none; border-radius: 8px; font-size: 16px; font-weight: bold; cursor: pointer; }
		.error { background: #ffe0e0; color: #c00; padding: 10px; border-radius: 6px; margin-bottom: 14px; text-align: center; font-size: 14px; }
	</style>
</head>
<body>
	<div class="login-card">
		<div class="logo">%s</div>
		<h1>%s</h1>
		<p>Admin Dashboard</p>
		%s
		<form method="POST">
			<input type="password" name="password" placeholder="Enter admin password" required autofocus>
			<button type="submit">Login</button>
		</form>
	</div>
</body>
</html>`, shopName, logoLetter, shopName, errHTML)
}

// --- Bot logic ---

func handleMessage(from, message string) string {
	shopName := os.Getenv("SHOP_NAME")
	if shopName == "" {
		shopName = "our store"
	}
	message = strings.TrimSpace(message)
	messageLower := strings.ToLower(message)

	if strings.HasPrefix(messageLower, "track ") || strings.HasPrefix(messageLower, "status ") {
		parts := strings.Fields(message)
		if len(parts) == 2 {
			orderID := strings.ToUpper(parts[1])
			_, items, status, paymentStatus, total, err := getOrderStatus(orderID)
			if err != nil {
				return fmt.Sprintf("❌ Order *#%s* not found. Please check the order ID and try again.", orderID)
			}
			statusEmoji := "⏳"
			switch status {
			case "ready":
				statusEmoji = "✅"
			case "out_for_delivery":
				statusEmoji = "🚚"
			case "completed":
				statusEmoji = "🎉"
			case "pending_payment":
				statusEmoji = "💳"
			}
			payInfo := ""
			if paymentStatus == "unpaid" {
				payInfo = "\n⚠️ *Payment pending* — please complete payment to confirm your order."
			}
			return fmt.Sprintf("📦 *Order #%s*\n\nItems: %s\nTotal: NGN %.0f\nStatus: %s *%s*%s", orderID, items, total, statusEmoji, strings.ToUpper(status), payInfo)
		}
	}

	if messageLower == "orders" || messageLower == "my orders" {
		orders, err := getOrdersByPhone(from)
		if err != nil || len(orders) == 0 {
			return "You have no past orders. Send *hi* to start ordering!"
		}
		var sb strings.Builder
		sb.WriteString("📦 *Your Recent Orders:*\n\n")
		for _, o := range orders {
			sb.WriteString(o + "\n\n")
		}
		sb.WriteString("To track an order, type *track ORDERID* e.g. track A3KX\n\nSend *hi* to place a new order.")
		return sb.String()
	}

	session, exists := sessions[from]
	if !exists || messageLower == "hi" || messageLower == "hello" || messageLower == "start" {
		if !isOpen() {
			return closedMessage()
		}
		sessions[from] = &Session{State: "GREETING"}
		session = sessions[from]
	}

	if messageLower == "back" {
		switch session.State {
		case "QUANTITY":
			if len(session.Cart) > 0 {
				session.Cart = session.Cart[:len(session.Cart)-1]
			}
			session.State = "BROWSING"
			return fmt.Sprintf("↩️ Went back!\n\n%s", buildBrowsingMessage(session))
		case "REMOVE_ITEM":
			session.State = "BROWSING"
			return fmt.Sprintf("↩️ Went back!\n\n%s", buildBrowsingMessage(session))
		case "DELIVERY_TYPE":
			session.State = "BROWSING"
			return fmt.Sprintf("↩️ Went back!\n\n%s", buildBrowsingMessage(session))
		case "DELIVERY_ADDRESS":
			session.State = "DELIVERY_TYPE"
			return fmt.Sprintf("↩️ Went back!\n\n%s", buildDeliveryTypeMessage())
		case "PICKUP":
			session.State = "DELIVERY_TYPE"
			return fmt.Sprintf("↩️ Went back!\n\n%s", buildDeliveryTypeMessage())
		case "CONFIRM":
			if session.DeliveryType == "delivery" {
				session.State = "DELIVERY_ADDRESS"
				return "↩️ Went back!\n\nPlease enter your delivery address:\n\nOr type *back* to go back."
			}
			session.State = "PICKUP"
			return fmt.Sprintf("↩️ Went back!\n\n%s", buildPickupMessage())
		default:
			return "Nothing to go back to. Send *hi* to start a new order."
		}
	}

	switch session.State {
	case "GREETING":
		session.State = "BROWSING"
		categories, _ := getCategories()
		if isReturningCustomer(from) {
			if len(categories) > 1 {
				return fmt.Sprintf("👋 Welcome back to *%s*!\n\n%s", shopName, buildCategoryMessage(categories))
			}
			products, _ := getProducts()
			return fmt.Sprintf("👋 Welcome back to *%s*!\n\n%s", shopName, buildCatalogMessage(products))
		}
		if len(categories) > 1 {
			return fmt.Sprintf("👋 Welcome to *%s*! We're glad to have you.\n\n%s", shopName, buildCategoryMessage(categories))
		}
		products, _ := getProducts()
		if len(products) == 0 {
			return "Sorry, our catalog is currently unavailable. Please try again later."
		}
		return fmt.Sprintf("👋 Welcome to *%s*! We're glad to have you.\n\n%s", shopName, buildCatalogMessage(products))

	case "BROWSING":
		categories, _ := getCategories()
		if len(categories) > 1 {
			for i, cat := range categories {
				if messageLower == fmt.Sprintf("%d", i+1) || messageLower == strings.ToLower(cat) {
					session.BrowsingCategory = cat
					products, _ := getProductsByCategory(cat)
					return buildCatalogMessage(products)
				}
			}
		}
		var products []Product
		if session.BrowsingCategory != "" {
			products, _ = getProductsByCategory(session.BrowsingCategory)
		} else {
			products, _ = getProducts()
		}
		for i, p := range products {
			if messageLower == fmt.Sprintf("%d", i+1) {
				session.State = "QUANTITY"
				session.Cart = append(session.Cart, CartItem{Product: p, Quantity: 0})
				if p.ImageURL != "" {
					go sendWhatsAppImage(from, p.ImageURL, fmt.Sprintf("%s — NGN %.0f", p.Name, p.Price))
				}
				return fmt.Sprintf("How many of *%s* would you like?\n\nReply with a number or type *back* to go back.", p.Name)
			}
		}
		if messageLower == "remove" || messageLower == "remove item" {
			if len(session.Cart) == 0 {
				return "Your cart is empty. Nothing to remove."
			}
			session.State = "REMOVE_ITEM"
			return buildCartRemoveMessage(session)
		}
		if messageLower == "done" || messageLower == "checkout" {
			if len(session.Cart) == 0 {
				return "Your cart is empty. Please select at least one item."
			}
			session.State = "DELIVERY_TYPE"
			return buildDeliveryTypeMessage()
		}
		if messageLower == "cart" || messageLower == "view cart" {
			if len(session.Cart) == 0 {
				return "Your cart is empty.\n\n" + buildBrowsingMessage(session)
			}
			return buildCartMessage(session)
		}
		return "Please reply with a number from the menu.\n\nType *done* to checkout, *cart* to view cart, *remove* to remove an item, or *back* to go back."

	case "REMOVE_ITEM":
		idx := 0
		fmt.Sscanf(messageLower, "%d", &idx)
		if idx < 1 || idx > len(session.Cart) {
			return fmt.Sprintf("Please enter a number between 1 and %d.\n\nOr type *back* to go back.", len(session.Cart))
		}
		removed := session.Cart[idx-1].Product.Name
		session.Cart = append(session.Cart[:idx-1], session.Cart[idx:]...)
		session.State = "BROWSING"
		return fmt.Sprintf("✅ *%s* removed!\n\n%s", removed, buildBrowsingMessage(session))

	case "QUANTITY":
		qty := 0
		fmt.Sscanf(messageLower, "%d", &qty)
		if qty <= 0 {
			return "Please enter a valid quantity (e.g. 1, 2, 3...)\n\nOr type *back* to go back."
		}
		session.Cart[len(session.Cart)-1].Quantity = qty
		session.State = "BROWSING"
		return fmt.Sprintf("✅ Added!\n\n%s", buildBrowsingMessage(session))

	case "DELIVERY_TYPE":
		if messageLower == "1" || messageLower == "pickup" {
			session.DeliveryType = "pickup"
			session.State = "PICKUP"
			return buildPickupMessage()
		} else if messageLower == "2" || messageLower == "delivery" {
			session.DeliveryType = "delivery"
			session.State = "DELIVERY_ADDRESS"
			return "📍 Please enter your delivery address:\n\nOr type *back* to go back."
		}
		return "Please reply *1* for Pickup or *2* for Delivery.\n\nOr type *back* to go back."

	case "DELIVERY_ADDRESS":
		if len(message) < 5 {
			return "Please enter a valid address (at least 5 characters).\n\nOr type *back* to go back."
		}
		session.DeliveryAddress = message
		session.State = "CONFIRM"
		return buildOrderSummary(session)

	case "PICKUP":
		slot := 0
		fmt.Sscanf(messageLower, "%d", &slot)
		if slot < 1 || slot > len(pickupSlots) {
			return "Please choose a valid slot number.\n\nOr type *back* to go back."
		}
		session.Pickup = pickupSlots[slot-1]
		session.State = "CONFIRM"
		return buildOrderSummary(session)

	case "CONFIRM":
		if messageLower == "yes" || messageLower == "confirm" {
			total := calculateTotal(session)
			orderID, err := saveOrder(from, session, total)
			if err != nil {
				log.Printf("Error saving order: %v", err)
				return "Sorry, there was an error processing your order. Please try again."
			}
			session.PendingOrderID = orderID
			session.State = "AWAITING_PAYMENT"
			totalKobo := int(total * 100)
			paymentLink, err := createPaystackPaymentLink(orderID, from, totalKobo)
			if err != nil {
				log.Printf("Error creating payment link: %v", err)
				return "Sorry, there was an error generating your payment link. Please try again."
			}
			return fmt.Sprintf(
				"💳 *Almost there!*\n\nYour order *#%s* is reserved.\n\n*Please complete payment to confirm:*\n\n%s\n\n⏰ Payment link expires in 30 minutes.\n\nOnce payment is confirmed, you'll receive a confirmation message automatically.",
				orderID, paymentLink,
			)
		} else if messageLower == "no" || messageLower == "cancel" {
			delete(sessions, from)
			return "Order cancelled. Send *hi* to start again."
		}
		return "Please reply *yes* to proceed to payment, *no* to cancel, or *back* to make changes."

	case "AWAITING_PAYMENT":
		return fmt.Sprintf(
			"⏳ *Waiting for payment...*\n\nYour order *#%s* is reserved pending payment.\n\nPlease complete payment using the link we sent you.\n\nOnce paid, you'll receive a confirmation automatically.\n\nType *track %s* to check your order status.",
			session.PendingOrderID, session.PendingOrderID,
		)

	case "DONE":
		return "Your order has been placed! Send *hi* to place a new order or type *orders* to see your order history."
	}

	return "Send *hi* to start ordering."
}

// --- Message builders ---

func buildBrowsingMessage(session *Session) string {
	var products []Product
	if session.BrowsingCategory != "" {
		products, _ = getProductsByCategory(session.BrowsingCategory)
	} else {
		products, _ = getProducts()
	}
	msg := buildCatalogMessage(products)
	if len(session.Cart) > 0 {
		msg += fmt.Sprintf("\n\n🛒 *Cart:* %d item(s) — type *cart* to view, *remove* to remove an item", len(session.Cart))
	}
	return msg
}

func buildCategoryMessage(categories []string) string {
	var sb strings.Builder
	sb.WriteString("🗂️ *Browse by category:*\n\n")
	for i, cat := range categories {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, cat))
	}
	sb.WriteString("\nReply with a category number to see products.")
	return sb.String()
}

func buildCatalogMessage(products []Product) string {
	if len(products) == 0 {
		return "No products available in this category."
	}
	var sb strings.Builder
	sb.WriteString("🛒 *Products:*\n\n")
	for i, p := range products {
		sb.WriteString(fmt.Sprintf("%d. %s — NGN %.0f\n", i+1, p.Name, p.Price))
	}
	sb.WriteString("\nReply with the *number* of the item you want.\nType *done* to checkout or *back* to go back.")
	return sb.String()
}

func buildCartMessage(session *Session) string {
	var sb strings.Builder
	sb.WriteString("🛒 *Your Cart:*\n\n")
	total := 0.0
	for i, item := range session.Cart {
		subtotal := item.Product.Price * float64(item.Quantity)
		sb.WriteString(fmt.Sprintf("%d. %s x%d — NGN %.0f\n", i+1, item.Product.Name, item.Quantity, subtotal))
		total += subtotal
	}
	sb.WriteString(fmt.Sprintf("\n*Subtotal: NGN %.0f*", total))
	sb.WriteString("\n\nType *done* to checkout, *remove* to remove an item, or continue shopping.")
	return sb.String()
}

func buildCartRemoveMessage(session *Session) string {
	var sb strings.Builder
	sb.WriteString("🗑️ *Which item would you like to remove?*\n\n")
	for i, item := range session.Cart {
		sb.WriteString(fmt.Sprintf("%d. %s x%d\n", i+1, item.Product.Name, item.Quantity))
	}
	sb.WriteString("\nReply with the number of the item to remove.\nOr type *back* to go back.")
	return sb.String()
}

func buildDeliveryTypeMessage() string {
	return fmt.Sprintf("🚚 *How would you like to receive your order?*\n\n1. 🏪 Pickup (Free)\n2. 🚚 Delivery (+NGN %.0f)\n\nReply *1* or *2*.\nType *back* to go back.", DeliveryFee)
}

func buildPickupMessage() string {
	var sb strings.Builder
	sb.WriteString("🕐 *Choose a pickup time:*\n\n")
	for i, slot := range pickupSlots {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, slot))
	}
	sb.WriteString("\nReply with the slot number.\nType *back* to go back.")
	return sb.String()
}

func buildOrderSummary(s *Session) string {
	var sb strings.Builder
	sb.WriteString("📋 *Order Summary:*\n\n")
	subtotal := 0.0
	for _, item := range s.Cart {
		itemTotal := item.Product.Price * float64(item.Quantity)
		sb.WriteString(fmt.Sprintf("• %s x%d — NGN %.0f\n", item.Product.Name, item.Quantity, itemTotal))
		subtotal += itemTotal
	}
	sb.WriteString(fmt.Sprintf("\nSubtotal: NGN %.0f", subtotal))
	if s.DeliveryType == "delivery" {
		sb.WriteString(fmt.Sprintf("\nDelivery fee: NGN %.0f", DeliveryFee))
		sb.WriteString(fmt.Sprintf("\n*Total: NGN %.0f*", subtotal+DeliveryFee))
		sb.WriteString(fmt.Sprintf("\n\n📍 Deliver to: %s", s.DeliveryAddress))
		sb.WriteString("\n⏱️ Estimated time: 30 minutes")
	} else {
		sb.WriteString(fmt.Sprintf("\n*Total: NGN %.0f*", subtotal))
		sb.WriteString(fmt.Sprintf("\n\n🏪 Pickup: %s", s.Pickup))
	}
	sb.WriteString("\n\nReply *yes* to proceed to payment, *no* to cancel, or *back* to make changes.")
	return sb.String()
}

func calculateTotal(session *Session) float64 {
	total := 0.0
	for _, item := range session.Cart {
		total += item.Product.Price * float64(item.Quantity)
	}
	if session.DeliveryType == "delivery" {
		total += DeliveryFee
	}
	return total
}

// --- Webhook handlers ---

// Twilio webhook
func twilioWebhookHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	from := strings.TrimPrefix(r.FormValue("From"), "whatsapp:")
	body := r.FormValue("Body")
	log.Printf("[Twilio] Message from %s: %s", from, body)
	reply := handleMessage(from, body)
	if err := sendWhatsAppMessage(from, reply); err != nil {
		log.Printf("Error sending message: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// Meta webhook verification
func metaWebhookHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		// Webhook verification
		verifyToken := os.Getenv("WEBHOOK_VERIFY_TOKEN")
		mode := r.URL.Query().Get("hub.mode")
		token := r.URL.Query().Get("hub.verify_token")
		challenge := r.URL.Query().Get("hub.challenge")

		if mode == "subscribe" && token == verifyToken {
			log.Println("✅ Meta webhook verified!")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, challenge)
			return
		}
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	if r.Method == http.MethodPost {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}

		w.WriteHeader(http.StatusOK)

		var event struct {
			Entry []struct {
				Changes []struct {
					Value struct {
						Messages []struct {
							From string `json:"from"`
							Text struct {
								Body string `json:"body"`
							} `json:"text"`
							Type string `json:"type"`
						} `json:"messages"`
					} `json:"value"`
				} `json:"changes"`
			} `json:"entry"`
		}

		if err := json.Unmarshal(body, &event); err != nil {
			log.Printf("Error parsing Meta webhook: %v", err)
			return
		}

		for _, entry := range event.Entry {
			for _, change := range entry.Changes {
				for _, msg := range change.Value.Messages {
					if msg.Type == "text" {
						from := msg.From
						text := msg.Text.Body
						log.Printf("[Meta] Message from %s: %s", from, text)
						reply := handleMessage(from, text)
						if err := sendWhatsAppMessage(from, reply); err != nil {
							log.Printf("Error sending Meta message: %v", err)
						}
					}
				}
			}
		}
	}
}

// Paystack webhook
func paystackWebhookHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	secretKey := os.Getenv("PAYSTACK_SECRET_KEY")
	mac := hmac.New(sha512.New, []byte(secretKey))
	mac.Write(body)
	expectedSig := fmt.Sprintf("%x", mac.Sum(nil))
	receivedSig := r.Header.Get("X-Paystack-Signature")

	if expectedSig != receivedSig {
		log.Printf("Invalid Paystack signature")
		http.Error(w, "Invalid signature", http.StatusUnauthorized)
		return
	}

	var event struct {
		Event string `json:"event"`
		Data  struct {
			Reference string `json:"reference"`
			Status    string `json:"status"`
			Metadata  struct {
				OrderID string `json:"order_id"`
				Phone   string `json:"phone"`
			} `json:"metadata"`
		} `json:"data"`
	}

	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)

	if event.Event == "charge.success" {
		orderID := event.Data.Metadata.OrderID
		phone := event.Data.Metadata.Phone

		db.Exec(`UPDATE orders SET payment_status = 'paid', status = 'pending' WHERE order_id = $1`, orderID)

		var items string
		var total float64
		var deliveryType, deliveryAddress, pickupSlot string
		db.QueryRow(`SELECT items, total, delivery_type, delivery_address, pickup_slot FROM orders WHERE order_id = $1`, orderID).
			Scan(&items, &total, &deliveryType, &deliveryAddress, &pickupSlot)

		go func() {
			fakeSession := &Session{
				DeliveryType:    deliveryType,
				DeliveryAddress: deliveryAddress,
				Pickup:          pickupSlot,
			}
			notifyAdmin(orderID, phone, items, total, fakeSession)
		}()

		go func() {
			shopName := os.Getenv("SHOP_NAME")
			if shopName == "" {
				shopName = "Supermarket"
			}
			pickupInfo := fmt.Sprintf("Pickup time: *%s*", pickupSlot)
			if deliveryType == "delivery" {
				pickupInfo = fmt.Sprintf("Delivery to: *%s*\n⏱️ Estimated time: 30 minutes", deliveryAddress)
			}
			msg := fmt.Sprintf(
				"🎉 *Payment confirmed!*\n\nThank you! Your order *#%s* at *%s* has been confirmed.\n\n%s\n\n📌 Type *track %s* to check your order status.\n\nWe'll notify you when your order is ready!",
				orderID, shopName, pickupInfo, orderID,
			)
			sendWhatsAppMessage(phone, msg)
			for _, session := range sessions {
				if session.PendingOrderID == orderID {
					session.State = "DONE"
				}
			}
		}()

		log.Printf("Payment confirmed for order %s", orderID)
	}
}

// --- Shared HTML header ---

func adminHeader(w http.ResponseWriter, activeTab string) {
	shopName := os.Getenv("SHOP_NAME")
	if shopName == "" {
		shopName = "Supermarket"
	}

	var totalProducts, totalOrders int
	db.QueryRow(`SELECT COUNT(*) FROM products`).Scan(&totalProducts)
	db.QueryRow(`SELECT COUNT(*) FROM orders`).Scan(&totalOrders)

	productsActive := ""
	ordersActive := ""
	if activeTab == "products" {
		productsActive = "active"
	} else {
		ordersActive = "active"
	}

	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head>
	<title>%s Admin</title>
	<meta name="viewport" content="width=device-width, initial-scale=1.0">
	<style>
		* { box-sizing: border-box; margin: 0; padding: 0; }
		body { font-family: 'Segoe UI', Arial, sans-serif; background: #f1f2f6; font-size: 14px; color: #333; }

		/* Layout shell */
		.shell { display: flex; min-height: 100vh; }

		/* Sidebar */
		.sidebar { width: 230px; background: #5c0e1e; color: #f1d9dd; flex-shrink: 0; display: flex; flex-direction: column; }
		.sidebar-brand { padding: 22px 20px; font-size: 22px; font-weight: 800; font-style: italic; color: #fff; border-bottom: 1px solid rgba(255,255,255,0.08); }
		.sidebar-nav { flex: 1; padding: 10px 0; }
		.nav-item { display: flex; align-items: center; gap: 10px; padding: 12px 20px; color: #e8c7cd; text-decoration: none; font-size: 14px; border-left: 3px solid transparent; }
		.nav-item:hover { background: rgba(255,255,255,0.06); }
		.nav-item.active { background: rgba(255,255,255,0.12); color: #fff; border-left: 3px solid #fff; font-weight: 600; }
		.nav-icon { width: 22px; height: 22px; display: inline-flex; align-items: center; justify-content: center; background: rgba(255,255,255,0.12); border-radius: 5px; font-size: 11px; font-weight: 700; flex-shrink: 0; }
		.sidebar-help { padding: 18px 20px; font-size: 12px; color: #c79aa1; border-top: 1px solid rgba(255,255,255,0.08); }
		.sidebar-help strong { display: block; color: #fff; margin-bottom: 4px; }

		/* Main area */
		.main { flex: 1; min-width: 0; display: flex; flex-direction: column; }
		.topbar { background: #fff; padding: 14px 24px; display: flex; justify-content: space-between; align-items: center; box-shadow: 0 1px 3px rgba(0,0,0,0.06); }
		.topbar-tabs { display: flex; gap: 8px; }
		.topbar-tab { padding: 8px 16px; border-radius: 6px; font-size: 13px; font-weight: 600; text-decoration: none; color: #5c0e1e; background: #f1e3e5; }
		.topbar-tab.active { background: #5c0e1e; color: #fff; }
		.topbar-right { display: flex; align-items: center; gap: 12px; }
		.logout-pill { background: #5c0e1e; color: #fff; padding: 8px 16px; border-radius: 6px; text-decoration: none; font-size: 13px; font-weight: 600; }

		.content { padding: 24px; max-width: 1200px; width: 100%%; margin: 0 auto; }

		/* Stat cards */
		.stats { display: grid; grid-template-columns: repeat(2, 1fr); gap: 16px; margin-bottom: 24px; }
		@media(min-width: 700px) { .stats { grid-template-columns: repeat(4, 1fr); } }
		.stat { background: linear-gradient(135deg, #7a1429, #5c0e1e); color: #fff; border-radius: 10px; padding: 18px; }
		.stat-value { font-size: 26px; font-weight: 800; }
		.stat-label { font-size: 12px; opacity: 0.85; margin-top: 4px; }

		/* Card */
		.card { background: #fff; border-radius: 10px; padding: 20px; margin-bottom: 20px; box-shadow: 0 1px 3px rgba(0,0,0,0.06); }
		.card h2 { margin-bottom: 14px; color: #333; font-size: 16px; }

		.search-bar { display: flex; gap: 10px; margin-bottom: 20px; }
		.search-bar input { flex: 1; padding: 10px; border: 1px solid #ddd; border-radius: 6px; font-size: 14px; }

		.form-row { display: flex; flex-direction: column; gap: 10px; }
		@media(min-width: 600px) { .form-row { flex-direction: row; } }
		input[type=text], input[type=number] { padding: 10px; border: 1px solid #ddd; border-radius: 6px; width: 100%%; font-size: 14px; }

		.btn { padding: 10px 18px; border: none; border-radius: 6px; cursor: pointer; font-size: 13px; font-weight: 700; width: 100%%; }
		@media(min-width: 600px) { .btn { width: auto; } }
		.btn-green { background: #1f9d55; color: white; }
		.btn-red { background: #d63447; color: white; }
		.btn-blue { background: #2563eb; color: white; }
		.btn-orange { background: #e07b13; color: white; }
		.btn-maroon { background: #5c0e1e; color: white; }
		.btn-sm { padding: 6px 12px; font-size: 12px; width: auto; }

		/* Table style list (products grouped) */
		.table-wrap { overflow-x: auto; }
		table.data-table { width: 100%%; border-collapse: collapse; font-size: 13px; }
		table.data-table th { text-align: left; padding: 10px 12px; background: #f7f1f2; color: #5c0e1e; font-size: 12px; text-transform: uppercase; letter-spacing: 0.03em; border-bottom: 2px solid #eee; white-space: nowrap; }
		table.data-table td { padding: 10px 12px; border-bottom: 1px solid #f0f0f0; vertical-align: middle; white-space: nowrap; }
		table.data-table tr:hover td { background: #fafafa; }
		.cat-pill { display: inline-block; background: #f1e3e5; color: #5c0e1e; padding: 3px 10px; border-radius: 20px; font-size: 11px; font-weight: 700; }
		.row-actions { display: flex; gap: 6px; flex-wrap: nowrap; }
		.price-edit { display: flex; gap: 4px; align-items: center; }
		.price-edit input { width: 90px; padding: 6px; font-size: 12px; }
		.unavailable-row td { opacity: 0.45; }
		.unavailable-row .prod-name { text-decoration: line-through; }

		.status-pill { padding: 4px 12px; border-radius: 20px; font-size: 11px; font-weight: 700; display: inline-block; }
		.status-pending, .status-pending_payment { background: #fdecea; color: #c0392b; }
		.status-ready { background: #e8f8ee; color: #1f9d55; }
		.status-out_for_delivery { background: #eaf1fe; color: #2563eb; }
		.status-completed { background: #eee; color: #555; }
		.pay-pill { padding: 3px 10px; border-radius: 20px; font-size: 10px; font-weight: 700; margin-left: 6px; }
		.pay-paid { background: #e8f8ee; color: #1f9d55; }
		.pay-unpaid { background: #fdecea; color: #c0392b; }
		.delivery-pill { padding: 3px 10px; border-radius: 20px; font-size: 10px; font-weight: 700; }
		.dp-delivery { background: #eaf1fe; color: #2563eb; }
		.dp-pickup { background: #e8f8ee; color: #1f9d55; }

		select { padding: 7px; border: 1px solid #ddd; border-radius: 6px; font-size: 13px; }
		.empty { text-align: center; padding: 30px; color: #999; }
	</style>
</head>
<body>
	<div class="shell">
		<div class="sidebar">
			<div class="sidebar-brand">%s</div>
			<div class="sidebar-nav">
				<a href="/admin" class="nav-item %s"><span class="nav-icon">D</span> Dashboard</a>
				<a href="/admin/orders" class="nav-item %s"><span class="nav-icon">O</span> Orders</a>
				<a href="/admin" class="nav-item %s"><span class="nav-icon">P</span> Products</a>
				<a href="#" class="nav-item"><span class="nav-icon">C</span> Categories</a>
				<a href="#" class="nav-item"><span class="nav-icon">S</span> Schedule</a>
				<a href="#" class="nav-item"><span class="nav-icon">I</span> Image Upload</a>
				<a href="#" class="nav-item"><span class="nav-icon">G</span> Configure</a>
			</div>
			<div class="sidebar-help">
				<strong>Need help?</strong>
				Contact support for assistance with your store.
			</div>
		</div>
		<div class="main">
			<div class="topbar">
				<div class="topbar-tabs">
					<a href="/admin" class="topbar-tab %s">Products</a>
					<a href="/admin/orders" class="topbar-tab %s">Orders</a>
				</div>
				<div class="topbar-right">
					<a href="/admin/logout" class="logout-pill">Logout</a>
				</div>
			</div>
			<div class="content">
				<div class="stats">
					<div class="stat"><div class="stat-value">%d</div><div class="stat-label">Total Products</div></div>
					<div class="stat"><div class="stat-value">%d</div><div class="stat-label">Total Orders</div></div>
					<div class="stat"><div class="stat-value">NGN</div><div class="stat-label">Currency</div></div>
					<div class="stat"><div class="stat-value">Live</div><div class="stat-label">Bot Status</div></div>
				</div>`, shopName, shopName, productsActive, ordersActive, productsActive, productsActive, ordersActive, totalProducts, totalOrders)
}

func adminFooter(w http.ResponseWriter) {
	fmt.Fprintf(w, `</div></div></div></body></html>`)
}

// --- Admin products handler ---

func adminHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		r.ParseMultipartForm(10 << 20) // 10MB max
		action := r.FormValue("action")
		switch action {
		case "clear_all_products":
			db.Exec(`DELETE FROM products`)
		case "add_product":
			name := r.FormValue("name")
			price := 0.0
			fmt.Sscanf(r.FormValue("price"), "%f", &price)
			category := r.FormValue("category")
			if category == "" {
				category = "General"
			}
			imageURL := ""
			file, header, err := r.FormFile("image")
			if err == nil {
				defer file.Close()
				uploadedURL, uploadErr := uploadToCloudinary(file, header.Filename)
				if uploadErr != nil {
					log.Printf("Cloudinary upload error: %v", uploadErr)
				} else {
					imageURL = uploadedURL
				}
			}
			if name != "" && price > 0 {
				db.Exec(`INSERT INTO products (name, price, category, image_url) VALUES ($1, $2, $3, $4)`, name, price, category, imageURL)
			}
		case "delete_product":
			id := r.FormValue("id")
			db.Exec(`DELETE FROM products WHERE id = $1`, id)
		case "toggle_product":
			id := r.FormValue("id")
			db.Exec(`UPDATE products SET available = NOT available WHERE id = $1`, id)
		case "edit_price":
			id := r.FormValue("id")
			price := 0.0
			fmt.Sscanf(r.FormValue("price"), "%f", &price)
			if price > 0 {
				db.Exec(`UPDATE products SET price = $1 WHERE id = $2`, price, id)
			}
		case "upload_image":
			id := r.FormValue("id")
			file, header, err := r.FormFile("image")
			if err == nil {
				defer file.Close()
				uploadedURL, uploadErr := uploadToCloudinary(file, header.Filename)
				if uploadErr != nil {
					log.Printf("Cloudinary upload error: %v", uploadErr)
				} else {
					db.Exec(`UPDATE products SET image_url = $1 WHERE id = $2`, uploadedURL, id)
				}
			}
		}
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}

	products, _ := getAllProducts()
	w.Header().Set("Content-Type", "text/html")
	adminHeader(w, "products")

	fmt.Fprintf(w, `
	<div class="card">
		<h2>Add New Product</h2>
		<form method="POST" action="/admin" enctype="multipart/form-data">
			<input type="hidden" name="action" value="add_product">
			<div class="form-row">
				<input type="text" name="name" placeholder="Product name e.g. Rice (5kg)" required>
				<input type="number" name="price" placeholder="Price in NGN" min="1" required>
				<input type="text" name="category" placeholder="Category e.g. Grains">
				<input type="file" name="image" accept="image/*">
				<button type="submit" class="btn btn-green">+ Add</button>
			</div>
		</form>
	</div>`)

	fmt.Fprintf(w, `
	<div class="card">
		<h2>All Products (%d)</h2>
		<form method="POST" action="/admin" style="margin-bottom:14px">
			<input type="hidden" name="action" value="clear_all_products">
			<button type="submit" class="btn btn-red" onclick="return confirm('This will permanently delete ALL products. Are you sure?')">🗑️ Clear All Products</button>
		</form>`, len(products))

	if len(products) == 0 {
		fmt.Fprintf(w, `<div class="empty">No products yet. Add your first product above!</div>`)
	} else {
		fmt.Fprintf(w, `<div class="table-wrap"><table class="data-table"><tr><th>S.L</th><th>Image</th><th>Product Name</th><th>Category</th><th>Price (NGN)</th><th>Status</th><th>Update Price</th><th>Actions</th></tr>`)

		for i, p := range products {
			rowClass := ""
			toggleLabel := "Hide"
			toggleClass := "btn-orange"
			statusPill := `<span class="status-pill status-ready">Visible</span>`
			if !p.Available {
				rowClass = "unavailable-row"
				toggleLabel = "Show"
				toggleClass = "btn-green"
				statusPill = `<span class="status-pill status-pending">Hidden</span>`
			}
			imageCell := `<span style="color:#bbb;font-size:11px">No image</span>`
			if p.ImageURL != "" {
				imageCell = fmt.Sprintf(`<img src="%s" style="width:42px;height:42px;object-fit:cover;border-radius:6px">`, p.ImageURL)
			}

			fmt.Fprintf(w, `
			<tr class="%s">
				<td>%02d</td>
				<td>
					%s
					<form method="POST" action="/admin" enctype="multipart/form-data" style="margin-top:4px">
						<input type="hidden" name="action" value="upload_image">
						<input type="hidden" name="id" value="%d">
						<input type="file" name="image" accept="image/*" style="font-size:10px;width:90px" onchange="this.form.submit()">
					</form>
				</td>
				<td class="prod-name">%s</td>
				<td><span class="cat-pill">%s</span></td>
				<td>NGN %.0f</td>
				<td>%s</td>
				<td>
					<form method="POST" action="/admin" class="price-edit">
						<input type="hidden" name="action" value="edit_price">
						<input type="hidden" name="id" value="%d">
						<input type="number" name="price" placeholder="New price" min="1">
						<button type="submit" class="btn btn-blue btn-sm">Update</button>
					</form>
				</td>
				<td>
					<div class="row-actions">
						<form method="POST" action="/admin">
							<input type="hidden" name="action" value="toggle_product">
							<input type="hidden" name="id" value="%d">
							<button type="submit" class="btn %s btn-sm">%s</button>
						</form>
						<form method="POST" action="/admin">
							<input type="hidden" name="action" value="delete_product">
							<input type="hidden" name="id" value="%d">
							<button type="submit" class="btn btn-red btn-sm" onclick="return confirm('Delete this product?')">Delete</button>
						</form>
					</div>
				</td>
			</tr>`, rowClass, i+1, imageCell, p.ID, p.Name, p.Category, p.Price, statusPill, p.ID, p.ID, toggleClass, toggleLabel, p.ID)
		}
		fmt.Fprintf(w, `</table></div>`)
	}
	fmt.Fprintf(w, `</div>`)
	adminFooter(w)
}

// --- Admin orders handler ---

func adminOrdersHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		r.ParseForm()
		orderID := r.FormValue("id")
		newStatus := r.FormValue("status")
		var phone string
		db.QueryRow(`SELECT phone FROM orders WHERE order_id = $1`, orderID).Scan(&phone)
		db.Exec(`UPDATE orders SET status = $1 WHERE order_id = $2`, newStatus, orderID)
		if phone != "" {
			go notifyCustomer(phone, orderID, newStatus)
		}
		http.Redirect(w, r, "/admin/orders", http.StatusSeeOther)
		return
	}

	search := r.URL.Query().Get("search")
	var totalOrders int
	var totalRevenue float64
	var pendingOrders int
	db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(total), 0) FROM orders WHERE created_at::date = CURRENT_DATE AND payment_status = 'paid'`).Scan(&totalOrders, &totalRevenue)
	db.QueryRow(`SELECT COUNT(*) FROM orders WHERE status = 'pending' AND payment_status = 'paid'`).Scan(&pendingOrders)

	var rows *sql.Rows
	var err error
	if search != "" {
		rows, err = db.Query(
			`SELECT order_id, phone, items, total, delivery_type, delivery_address, pickup_slot, status, payment_status, created_at FROM orders WHERE order_id ILIKE $1 OR phone ILIKE $1 ORDER BY created_at DESC`,
			"%"+search+"%",
		)
	} else {
		rows, err = db.Query(`SELECT order_id, phone, items, total, delivery_type, delivery_address, pickup_slot, status, payment_status, created_at FROM orders ORDER BY created_at DESC`)
	}
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type Order struct {
		OrderID         string
		Phone           string
		Items           string
		Total           float64
		DeliveryType    string
		DeliveryAddress string
		PickupSlot      string
		Status          string
		PaymentStatus   string
		CreatedAt       string
	}

	var orders []Order
	for rows.Next() {
		var o Order
		rows.Scan(&o.OrderID, &o.Phone, &o.Items, &o.Total, &o.DeliveryType, &o.DeliveryAddress, &o.PickupSlot, &o.Status, &o.PaymentStatus, &o.CreatedAt)
		orders = append(orders, o)
	}

	w.Header().Set("Content-Type", "text/html")
	adminHeader(w, "orders")

	fmt.Fprintf(w, `
	<div class="stats">
		<div class="stat"><div class="stat-value">%d</div><div class="stat-label">Today's Orders</div></div>
		<div class="stat"><div class="stat-value">NGN %.0f</div><div class="stat-label">Today's Revenue</div></div>
		<div class="stat"><div class="stat-value">%d</div><div class="stat-label">Pending Orders</div></div>
		<div class="stat"><div class="stat-value">%d</div><div class="stat-label">Total Orders</div></div>
	</div>`, totalOrders, totalRevenue, pendingOrders, len(orders))
	clearBtn := ""
	if search != "" {
		clearBtn = `<a href="/admin/orders" class="btn btn-red" style="width:auto;padding:10px 18px;text-decoration:none;display:inline-block">Clear</a>`
	}
	fmt.Fprintf(w, `
	<div class="search-bar">
		<form method="GET" action="/admin/orders" style="display:flex;gap:10px;flex:1">
			<input type="text" name="search" placeholder="Search by order ID or phone..." value="%s">
			<button type="submit" class="btn btn-green" style="width:auto">Search</button>
			%s
		</form>
	</div>`, search, clearBtn)

	fmt.Fprintf(w, `<div class="card"><h2>Orders (%d)</h2>`, len(orders))

	if len(orders) == 0 {
		fmt.Fprintf(w, `<div class="empty">No orders found</div></div>`)
	} else {
		fmt.Fprintf(w, `<div class="table-wrap"><table class="data-table"><tr><th>S.L</th><th>Order ID</th><th>Items</th><th>Total</th><th>Type</th><th>Phone</th><th>Time</th><th>Payment</th><th>Status</th><th>Update</th></tr>`)

		for i, o := range orders {
			deliveryInfo := o.PickupSlot
			deliveryPill := `<span class="delivery-pill dp-pickup">Pickup</span>`
			statusOptions := `
				<option value="pending" ` + selected(o.Status, "pending") + `>Pending</option>
				<option value="ready" ` + selected(o.Status, "ready") + `>Ready for pickup</option>
				<option value="completed" ` + selected(o.Status, "completed") + `>Completed</option>`

			if o.DeliveryType == "delivery" {
				deliveryInfo = o.DeliveryAddress
				deliveryPill = `<span class="delivery-pill dp-delivery">Delivery</span>`
				statusOptions = `
				<option value="pending" ` + selected(o.Status, "pending") + `>Pending</option>
				<option value="out_for_delivery" ` + selected(o.Status, "out_for_delivery") + `>Out for delivery</option>
				<option value="completed" ` + selected(o.Status, "completed") + `>Completed</option>`
			}

			payClass := "pay-unpaid"
			if o.PaymentStatus == "paid" {
				payClass = "pay-paid"
			}

			statusForm := `<span style="color:#999;font-size:12px">Awaiting payment</span>`
			if o.PaymentStatus == "paid" {
				statusForm = fmt.Sprintf(`
				<form method="POST" action="/admin/orders" style="display:flex;gap:6px;align-items:center">
					<input type="hidden" name="id" value="%s">
					<select name="status">%s</select>
					<button type="submit" class="btn btn-maroon btn-sm">Update</button>
				</form>`, o.OrderID, statusOptions)
			}

			fmt.Fprintf(w, `
			<tr>
				<td>%02d</td>
				<td><strong>%s</strong> %s</td>
				<td>%s</td>
				<td>NGN %.0f</td>
				<td>%s<br><span style="font-size:11px;color:#888">%s</span></td>
				<td>%s</td>
				<td>%s</td>
				<td><span class="pay-pill %s">%s</span></td>
				<td><span class="status-pill status-%s">%s</span></td>
				<td>%s</td>
			</tr>`,
				i+1, o.OrderID, deliveryPill, o.Items, o.Total,
				deliveryInfo, func() string {
					if o.DeliveryType == "delivery" {
						return "Delivery"
					}
					return "Pickup"
				}(),
				o.Phone, o.CreatedAt,
				payClass, o.PaymentStatus,
				o.Status, o.Status,
				statusForm,
			)
		}
		fmt.Fprintf(w, `</table></div></div>`)
	}

	fmt.Fprintf(w, `</div>`)
	adminFooter(w)
}

func selected(current, value string) string {
	if current == value {
		return "selected"
	}
	return ""
}

// --- Logout handler ---

func logoutHandler(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:   "admin_session",
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// --- Entry point ---

func main() {
	initDB()
	defer db.Close()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Both Twilio and Meta webhooks
	http.HandleFunc("/webhook", twilioWebhookHandler)
	http.HandleFunc("/meta/webhook", metaWebhookHandler)
	http.HandleFunc("/paystack/webhook", paystackWebhookHandler)
	http.HandleFunc("/admin", requireAuth(adminHandler))
	http.HandleFunc("/admin/orders", requireAuth(adminOrdersHandler))
	http.HandleFunc("/admin/logout", logoutHandler)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})

	addr := "0.0.0.0:" + port
	log.Printf("🚀 Bot server running on %s", addr)
	log.Printf("📊 Admin dashboard at http://localhost:%s/admin", port)
	log.Fatal(http.ListenAndServe(addr, nil))
}
