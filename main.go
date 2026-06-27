package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strings"

	_ "github.com/lib/pq"
)

// --- Data Models ---

type Product struct {
	ID    int
	Name  string
	Price float64
}

type CartItem struct {
	Product  Product
	Quantity int
}

type Session struct {
	State           string
	PreviousState   string
	Cart            []CartItem
	Pickup          string
	DeliveryType    string // "pickup" or "delivery"
	DeliveryAddress string
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
		status TEXT DEFAULT 'pending',
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
		available BOOLEAN DEFAULT true
	);`)
	if err != nil {
		log.Fatal("Failed to create products table:", err)
	}

	// Add missing columns if they don't exist (for existing databases)
	db.Exec(`ALTER TABLE orders ADD COLUMN IF NOT EXISTS delivery_type TEXT DEFAULT 'pickup'`)
	db.Exec(`ALTER TABLE orders ADD COLUMN IF NOT EXISTS delivery_address TEXT DEFAULT ''`)

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

// --- Product helpers ---

func getProducts() ([]Product, error) {
	rows, err := db.Query(`SELECT id, name, price FROM products WHERE available = true ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var products []Product
	for rows.Next() {
		var p Product
		rows.Scan(&p.ID, &p.Name, &p.Price)
		products = append(products, p)
	}
	return products, nil
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
		`INSERT INTO orders (order_id, phone, items, total, delivery_type, delivery_address, pickup_slot) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		orderID, phone, itemsStr, total, session.DeliveryType, session.DeliveryAddress, session.Pickup,
	)
	if err != nil {
		return "", err
	}

	return orderID, nil
}

func getOrdersByPhone(phone string) ([]string, error) {
	rows, err := db.Query(
		`SELECT order_id, items, total, delivery_type, pickup_slot, status, created_at FROM orders WHERE phone = $1 ORDER BY created_at DESC LIMIT 5`,
		phone,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var orderID, items, deliveryType, pickupSlot, status, createdAt string
		var total float64
		rows.Scan(&orderID, &items, &total, &deliveryType, &pickupSlot, &status, &createdAt)
		delivery := pickupSlot
		if deliveryType == "delivery" {
			delivery = "Delivery"
		}
		lines = append(lines, fmt.Sprintf("• *%s* — %s\n  ₦%.0f | %s | _%s_", orderID, items, total, delivery, status))
	}
	return lines, nil
}

// --- Twilio sender ---

func sendWhatsAppMessage(to, body string) error {
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

// --- Notification helpers ---

func notifyAdmin(orderID, phone, items string, total float64, session *Session) {
	adminPhone := os.Getenv("ADMIN_PHONE")
	shopName := os.Getenv("SHOP_NAME")
	if shopName == "" {
		shopName = "Supermarket"
	}
	if adminPhone == "" {
		return
	}

	deliveryInfo := fmt.Sprintf("Pickup: %s", session.Pickup)
	if session.DeliveryType == "delivery" {
		deliveryInfo = fmt.Sprintf("Delivery to: %s", session.DeliveryAddress)
	}

	msg := fmt.Sprintf(
		"🔔 *New Order - %s*\n\nOrder ID: *%s*\nCustomer: %s\nItems: %s\nTotal: ₦%.0f\n%s\n\nUpdate status at: %s/admin/orders",
		shopName, orderID, phone, items, total, deliveryInfo,
		os.Getenv("RAILWAY_PUBLIC_DOMAIN"),
	)
	if err := sendWhatsAppMessage(adminPhone, msg); err != nil {
		log.Printf("Failed to notify admin: %v", err)
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
		msg = fmt.Sprintf(
			"✅ *Your order is ready!*\n\nHi! Your order *#%s* at %s is ready for pickup.\n\nPlease come collect your items. Thank you! 🛒",
			orderID, shopName,
		)
	case "completed":
		msg = fmt.Sprintf(
			"🎉 *Order Completed!*\n\nYour order *#%s* has been marked as completed.\n\nThank you for shopping at %s! We hope to see you again soon. 😊",
			orderID, shopName,
		)
	case "out_for_delivery":
		msg = fmt.Sprintf(
			"🚚 *Your order is on the way!*\n\nYour order *#%s* from %s is out for delivery.\n\nEstimated arrival: *30 minutes* ⏱️\n\nPlease be available to receive your items!",
			orderID, shopName,
		)
	}

	if msg != "" {
		if err := sendWhatsAppMessage(phone, msg); err != nil {
			log.Printf("Failed to notify customer: %v", err)
		}
	}
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
		.logo { text-align: center; font-size: 40px; margin-bottom: 10px; }
		h1 { text-align: center; color: #333; font-size: 20px; margin-bottom: 6px; }
		p { text-align: center; color: #888; font-size: 14px; margin-bottom: 24px; }
		input[type=password] { width: 100%%; padding: 12px; border: 1px solid #ddd; border-radius: 8px; font-size: 15px; margin-bottom: 14px; }
		button { width: 100%%; padding: 12px; background: #4CAF50; color: white; border: none; border-radius: 8px; font-size: 16px; font-weight: bold; cursor: pointer; }
		.error { background: #ffe0e0; color: #c00; padding: 10px; border-radius: 6px; margin-bottom: 14px; text-align: center; font-size: 14px; }
	</style>
</head>
<body>
	<div class="login-card">
		<div class="logo">🛒</div>
		<h1>%s</h1>
		<p>Admin Dashboard</p>
		%s
		<form method="POST">
			<input type="password" name="password" placeholder="Enter admin password" required autofocus>
			<button type="submit">Login</button>
		</form>
	</div>
</body>
</html>`, shopName, shopName, errHTML)
}

// --- Bot logic ---

func handleMessage(from, message string) string {
	shopName := os.Getenv("SHOP_NAME")
	if shopName == "" {
		shopName = "our store"
	}

	message = strings.TrimSpace(message)
	messageLower := strings.ToLower(message)

	// Order history command
	if messageLower == "orders" || messageLower == "my orders" || messageLower == "order history" {
		orders, err := getOrdersByPhone(from)
		if err != nil || len(orders) == 0 {
			return "You have no past orders. Send *hi* to start ordering!"
		}
		var sb strings.Builder
		sb.WriteString("📦 *Your Recent Orders:*\n\n")
		for _, o := range orders {
			sb.WriteString(o + "\n\n")
		}
		sb.WriteString("Send *hi* to place a new order.")
		return sb.String()
	}

	session, exists := sessions[from]
	if !exists || messageLower == "hi" || messageLower == "hello" || messageLower == "start" {
		sessions[from] = &Session{State: "GREETING"}
		session = sessions[from]
	}

	// Handle back navigation
	if messageLower == "back" {
		switch session.State {
		case "QUANTITY":
			// Remove the last item added to cart
			if len(session.Cart) > 0 {
				session.Cart = session.Cart[:len(session.Cart)-1]
			}
			session.State = "BROWSING"
			products, _ := getProducts()
			return fmt.Sprintf("↩️ Went back!\n\n%s", buildCatalogMessage(products))
		case "DELIVERY_TYPE":
			session.State = "BROWSING"
			products, _ := getProducts()
			return fmt.Sprintf("↩️ Went back!\n\n%s", buildCatalogMessage(products))
		case "DELIVERY_ADDRESS":
			session.State = "DELIVERY_TYPE"
			return fmt.Sprintf("↩️ Went back!\n\n%s", buildDeliveryTypeMessage())
		case "PICKUP":
			session.State = "DELIVERY_TYPE"
			return fmt.Sprintf("↩️ Went back!\n\n%s", buildDeliveryTypeMessage())
		case "CONFIRM":
			if session.DeliveryType == "delivery" {
				session.State = "DELIVERY_ADDRESS"
				return "↩️ Went back!\n\nPlease enter your delivery address:"
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
		products, err := getProducts()
		if err != nil || len(products) == 0 {
			return "Sorry, our catalog is currently unavailable. Please try again later."
		}
		return fmt.Sprintf("👋 Welcome to *%s*!\n\n%s", shopName, buildCatalogMessage(products))

	case "BROWSING":
		products, _ := getProducts()
		for i, p := range products {
			if messageLower == fmt.Sprintf("%d", i+1) {
				session.State = "QUANTITY"
				session.Cart = append(session.Cart, CartItem{Product: p, Quantity: 0})
				return fmt.Sprintf("How many of *%s* would you like?\n\nReply with a number or type *back* to go back.", p.Name)
			}
		}
		if messageLower == "done" || messageLower == "checkout" {
			if len(session.Cart) == 0 {
				return "Your cart is empty. Please select at least one item."
			}
			session.State = "DELIVERY_TYPE"
			return buildDeliveryTypeMessage()
		}
		return "Please reply with a number from the menu, or type *done* to checkout.\n\nType *back* to go back."

	case "QUANTITY":
		qty := 0
		fmt.Sscanf(messageLower, "%d", &qty)
		if qty <= 0 {
			return "Please enter a valid quantity (e.g. 1, 2, 3...)\n\nOr type *back* to go back."
		}
		session.Cart[len(session.Cart)-1].Quantity = qty
		session.State = "BROWSING"
		products, _ := getProducts()
		return fmt.Sprintf("✅ Added! Reply with another number to add more items, or type *done* to checkout.\n\nType *back* to remove last item.\n\n%s", buildCatalogMessage(products))

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

			var itemParts []string
			for _, item := range session.Cart {
				itemParts = append(itemParts, fmt.Sprintf("%s x%d", item.Product.Name, item.Quantity))
			}
			itemsStr := strings.Join(itemParts, ", ")

			orderID, err := saveOrder(from, session, total)
			if err != nil {
				log.Printf("Error saving order: %v", err)
				return "Sorry, there was an error saving your order. Please try again."
			}

			go notifyAdmin(orderID, from, itemsStr, total, session)

			session.State = "DONE"

			if session.DeliveryType == "delivery" {
				return fmt.Sprintf(
					"🎉 *Order confirmed!*\n\nYour order ID is *%s*\n🚚 Delivery to: %s\n⏱️ Estimated time: *30 minutes*\n\n📌 *Please save your order ID* — you'll need it to track your order.\n\nWe'll send you a message when your order is on the way!\n\nType *orders* anytime to see your order history.",
					orderID, session.DeliveryAddress,
				)
			}
			return fmt.Sprintf(
				"🎉 *Order confirmed!*\n\nYour order ID is *%s*\nPickup time: *%s*\n\n📌 *Please save your order ID* — you'll need it to track your order.\n\nWe'll send you a message when your order is ready for pickup!\n\nType *orders* anytime to see your order history.",
				orderID, session.Pickup,
			)
		} else if messageLower == "no" || messageLower == "cancel" {
			delete(sessions, from)
			return "Order cancelled. Send *hi* to start again."
		}
		return "Please reply *yes* to confirm or *no* to cancel.\n\nOr type *back* to make changes."

	case "DONE":
		return "Your order has been placed! Send *hi* to place a new order or type *orders* to see your order history."
	}

	return "Send *hi* to start ordering."
}

// --- Helpers ---

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

func buildCatalogMessage(products []Product) string {
	var sb strings.Builder
	sb.WriteString("🛒 *Our Products:*\n\n")
	for i, p := range products {
		sb.WriteString(fmt.Sprintf("%d. %s — ₦%.0f\n", i+1, p.Name, p.Price))
	}
	sb.WriteString("\nReply with the *number* of the item you want.\nType *done* when ready to checkout.\nType *back* to go back.")
	return sb.String()
}

func buildDeliveryTypeMessage() string {
	return fmt.Sprintf("🚚 *How would you like to receive your order?*\n\n1. 🏪 Pickup (Free)\n2. 🚚 Delivery (+₦%.0f)\n\nReply *1* or *2*.\nType *back* to go back.", DeliveryFee)
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
		sb.WriteString(fmt.Sprintf("• %s x%d — ₦%.0f\n", item.Product.Name, item.Quantity, itemTotal))
		subtotal += itemTotal
	}
	sb.WriteString(fmt.Sprintf("\nSubtotal: ₦%.0f", subtotal))
	if s.DeliveryType == "delivery" {
		sb.WriteString(fmt.Sprintf("\nDelivery fee: ₦%.0f", DeliveryFee))
		sb.WriteString(fmt.Sprintf("\n*Total: ₦%.0f*", subtotal+DeliveryFee))
		sb.WriteString(fmt.Sprintf("\n\n📍 Deliver to: %s", s.DeliveryAddress))
		sb.WriteString("\n⏱️ Estimated time: 30 minutes")
	} else {
		sb.WriteString(fmt.Sprintf("\n*Total: ₦%.0f*", subtotal))
		sb.WriteString(fmt.Sprintf("\n\n🏪 Pickup: %s", s.Pickup))
	}
	sb.WriteString("\n\nReply *yes* to confirm, *no* to cancel, or *back* to make changes.")
	return sb.String()
}

// --- Shared HTML header ---

func adminHeader(w http.ResponseWriter, activeTab string) {
	shopName := os.Getenv("SHOP_NAME")
	if shopName == "" {
		shopName = "Supermarket"
	}

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
		body { font-family: Arial, sans-serif; background: #f5f5f5; font-size: 15px; }
		.header { background: #4CAF50; color: white; padding: 16px; display: flex; justify-content: space-between; align-items: center; }
		.header h1 { font-size: 20px; }
		.logout { color: white; font-size: 13px; text-decoration: none; background: rgba(0,0,0,0.2); padding: 6px 12px; border-radius: 6px; }
		.tabs { display: flex; gap: 8px; padding: 16px; background: white; border-bottom: 1px solid #eee; }
		.tab { flex: 1; text-align: center; padding: 10px; background: #f5f5f5; border: 2px solid #4CAF50; border-radius: 6px; color: #4CAF50; font-weight: bold; text-decoration: none; font-size: 14px; }
		.tab.active { background: #4CAF50; color: white; }
		.container { padding: 16px; max-width: 900px; margin: auto; }
		.card { background: white; border-radius: 8px; padding: 16px; margin-bottom: 16px; box-shadow: 0 1px 3px rgba(0,0,0,0.1); }
		.card h2 { margin-bottom: 12px; color: #333; font-size: 16px; }
		.form-row { display: flex; flex-direction: column; gap: 10px; }
		@media(min-width: 600px) { .form-row { flex-direction: row; } }
		input[type=text], input[type=number] { padding: 10px; border: 1px solid #ddd; border-radius: 6px; width: 100%%; font-size: 15px; }
		.btn { padding: 10px 18px; border: none; border-radius: 6px; cursor: pointer; font-size: 14px; font-weight: bold; width: 100%%; }
		@media(min-width: 600px) { .btn { width: auto; } }
		.btn-green { background: #4CAF50; color: white; }
		.btn-red { background: #f44336; color: white; }
		.product-item { display: flex; justify-content: space-between; align-items: center; padding: 12px 0; border-bottom: 1px solid #eee; gap: 10px; }
		.product-item:last-child { border-bottom: none; }
		.product-info { flex: 1; }
		.product-name { font-weight: bold; font-size: 15px; }
		.product-price { color: #4CAF50; font-size: 14px; margin-top: 2px; }
		.order-card { border: 1px solid #eee; border-radius: 8px; padding: 14px; margin-bottom: 12px; background: white; }
		.order-header { display: flex; justify-content: space-between; align-items: center; margin-bottom: 8px; flex-wrap: wrap; gap: 6px; }
		.order-id { font-weight: bold; font-size: 16px; color: #333; }
		.badge { padding: 4px 10px; border-radius: 20px; font-size: 12px; font-weight: bold; }
		.badge-pending { background: #fff3cd; color: #856404; }
		.badge-ready { background: #d4edda; color: #155724; }
		.badge-out_for_delivery { background: #fff3cd; color: #856404; }
		.badge-completed { background: #cce5ff; color: #004085; }
		.order-details { font-size: 14px; color: #555; margin-bottom: 10px; line-height: 1.6; }
		.order-total { font-weight: bold; color: #333; }
		.delivery-badge { display: inline-block; padding: 2px 8px; border-radius: 10px; font-size: 11px; font-weight: bold; margin-left: 6px; }
		.delivery { background: #e3f2fd; color: #1565c0; }
		.pickup { background: #e8f5e9; color: #2e7d32; }
		.status-form { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
		select { padding: 8px; border: 1px solid #ddd; border-radius: 6px; font-size: 14px; flex: 1; }
		.empty { text-align: center; padding: 30px; color: #888; }
	</style>
</head>
<body>
	<div class="header">
		<h1>🛒 %s</h1>
		<a href="/admin/logout" class="logout">Logout</a>
	</div>
	<div class="tabs">
		<a href="/admin" class="tab %s">📦 Products</a>
		<a href="/admin/orders" class="tab %s">🧾 Orders</a>
	</div>
	<div class="container">`, shopName, shopName, productsActive, ordersActive)
}

// --- Webhook handler ---

func webhookHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	from := r.FormValue("From")
	body := r.FormValue("Body")
	from = strings.TrimPrefix(from, "whatsapp:")

	log.Printf("Message from %s: %s", from, body)

	reply := handleMessage(from, body)

	if err := sendWhatsAppMessage(from, reply); err != nil {
		log.Printf("Error sending message: %v", err)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// --- Admin products handler ---

func adminHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		r.ParseForm()
		action := r.FormValue("action")
		switch action {
		case "add_product":
			name := r.FormValue("name")
			price := 0.0
			fmt.Sscanf(r.FormValue("price"), "%f", &price)
			if name != "" && price > 0 {
				db.Exec(`INSERT INTO products (name, price) VALUES ($1, $2)`, name, price)
			}
		case "delete_product":
			id := r.FormValue("id")
			db.Exec(`UPDATE products SET available = false WHERE id = $1`, id)
		}
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}

	products, _ := getProducts()

	w.Header().Set("Content-Type", "text/html")
	adminHeader(w, "products")

	fmt.Fprintf(w, `
	<div class="card">
		<h2>Add New Product</h2>
		<form method="POST" action="/admin">
			<input type="hidden" name="action" value="add_product">
			<div class="form-row">
				<input type="text" name="name" placeholder="Product name e.g. Rice (5kg)" required>
				<input type="number" name="price" placeholder="Price in ₦" min="1" required>
				<button type="submit" class="btn btn-green">+ Add Product</button>
			</div>
		</form>
	</div>
	<div class="card">
		<h2>Products (%d)</h2>`, len(products))

	if len(products) == 0 {
		fmt.Fprintf(w, `<div class="empty">No products yet. Add your first product above!</div>`)
	}

	for i, p := range products {
		fmt.Fprintf(w, `
		<div class="product-item">
			<div class="product-info">
				<div class="product-name">%d. %s</div>
				<div class="product-price">₦%.0f</div>
			</div>
			<form method="POST" action="/admin">
				<input type="hidden" name="action" value="delete_product">
				<input type="hidden" name="id" value="%d">
				<button type="submit" class="btn btn-red" onclick="return confirm('Delete this product?')">Delete</button>
			</form>
		</div>`, i+1, p.Name, p.Price, p.ID)
	}

	fmt.Fprintf(w, `</div></div></body></html>`)
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

	rows, err := db.Query(`SELECT order_id, phone, items, total, delivery_type, delivery_address, pickup_slot, status, created_at FROM orders ORDER BY created_at DESC`)
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
		CreatedAt       string
	}

	var orders []Order
	for rows.Next() {
		var o Order
		rows.Scan(&o.OrderID, &o.Phone, &o.Items, &o.Total, &o.DeliveryType, &o.DeliveryAddress, &o.PickupSlot, &o.Status, &o.CreatedAt)
		orders = append(orders, o)
	}

	w.Header().Set("Content-Type", "text/html")
	adminHeader(w, "orders")

	fmt.Fprintf(w, `<div class="card"><h2>Orders (%d)</h2></div>`, len(orders))

	if len(orders) == 0 {
		fmt.Fprintf(w, `<div class="empty">No orders yet</div>`)
	}

	for _, o := range orders {
		badgeClass := "badge-pending"
		if o.Status == "ready" {
			badgeClass = "badge-ready"
		} else if o.Status == "completed" {
			badgeClass = "badge-completed"
		} else if o.Status == "out_for_delivery" {
			badgeClass = "badge-out_for_delivery"
		}

		deliveryInfo := fmt.Sprintf("🏪 Pickup: %s", o.PickupSlot)
		deliveryBadge := `<span class="delivery-badge pickup">Pickup</span>`
		statusOptions := `
			<option value="pending" ` + selected(o.Status, "pending") + `>Pending</option>
			<option value="ready" ` + selected(o.Status, "ready") + `>Ready for pickup</option>
			<option value="completed" ` + selected(o.Status, "completed") + `>Completed</option>`

		if o.DeliveryType == "delivery" {
			deliveryInfo = fmt.Sprintf("🚚 Deliver to: %s", o.DeliveryAddress)
			deliveryBadge = `<span class="delivery-badge delivery">Delivery</span>`
			statusOptions = `
			<option value="pending" ` + selected(o.Status, "pending") + `>Pending</option>
			<option value="out_for_delivery" ` + selected(o.Status, "out_for_delivery") + `>Out for delivery</option>
			<option value="completed" ` + selected(o.Status, "completed") + `>Completed</option>`
		}

		fmt.Fprintf(w, `
		<div class="order-card">
			<div class="order-header">
				<span class="order-id">Order #%s %s</span>
				<span class="badge %s">%s</span>
			</div>
			<div class="order-details">
				📱 %s<br>
				🛍️ %s<br>
				💰 <span class="order-total">₦%.0f</span><br>
				%s<br>
				📅 %s
			</div>
			<form method="POST" action="/admin/orders" class="status-form">
				<input type="hidden" name="id" value="%s">
				<select name="status">%s</select>
				<button type="submit" class="btn btn-green">Update</button>
			</form>
		</div>`,
			o.OrderID, deliveryBadge, badgeClass, o.Status,
			o.Phone, o.Items, o.Total, deliveryInfo, o.CreatedAt,
			o.OrderID, statusOptions,
		)
	}

	fmt.Fprintf(w, `</div></body></html>`)
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

	http.HandleFunc("/webhook", webhookHandler)
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
