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
		category TEXT DEFAULT 'General',
		available BOOLEAN DEFAULT true
	);`)
	if err != nil {
		log.Fatal("Failed to create products table:", err)
	}

	// Add missing columns for existing databases
	db.Exec(`ALTER TABLE orders ADD COLUMN IF NOT EXISTS delivery_type TEXT DEFAULT 'pickup'`)
	db.Exec(`ALTER TABLE orders ADD COLUMN IF NOT EXISTS delivery_address TEXT DEFAULT ''`)
	db.Exec(`ALTER TABLE products ADD COLUMN IF NOT EXISTS category TEXT DEFAULT 'General'`)

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
		return true // No hours set, always open
	}
	now := time.Now()
	hour := now.Hour()
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
	rows, err := db.Query(`SELECT id, name, price, category, available FROM products WHERE available = true ORDER BY category, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var products []Product
	for rows.Next() {
		var p Product
		rows.Scan(&p.ID, &p.Name, &p.Price, &p.Category, &p.Available)
		products = append(products, p)
	}
	return products, nil
}

func getAllProducts() ([]Product, error) {
	rows, err := db.Query(`SELECT id, name, price, category, available FROM products ORDER BY category, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var products []Product
	for rows.Next() {
		var p Product
		rows.Scan(&p.ID, &p.Name, &p.Price, &p.Category, &p.Available)
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
	rows, err := db.Query(`SELECT id, name, price, category, available FROM products WHERE available = true AND category = $1 ORDER BY id`, category)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var products []Product
	for rows.Next() {
		var p Product
		rows.Scan(&p.ID, &p.Name, &p.Price, &p.Category, &p.Available)
		products = append(products, p)
	}
	return products, nil
}

func isReturningCustomer(phone string) bool {
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM orders WHERE phone = $1`, phone).Scan(&count)
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

func getOrderStatus(orderID string) (string, string, string, float64, error) {
	var phone, items, status string
	var total float64
	err := db.QueryRow(
		`SELECT phone, items, status, total FROM orders WHERE order_id = $1`,
		strings.ToUpper(orderID),
	).Scan(&phone, &items, &status, &total)
	return phone, items, status, total, err
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
	shopName := os.Getenv("SHOP_NAME")
	if shopName == "" {
		shopName = "Supermarket"
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

	// Support multiple admin numbers separated by comma
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
		msg = fmt.Sprintf(
			"✅ *Your order is ready!*\n\nHi! Your order *#%s* at %s is ready for pickup.\n\nPlease come collect your items. Thank you! 🛒",
			orderID, shopName,
		)
	case "out_for_delivery":
		msg = fmt.Sprintf(
			"🚚 *Your order is on the way!*\n\nYour order *#%s* from %s is out for delivery.\n\nEstimated arrival: *30 minutes* ⏱️\n\nPlease be available to receive your items!",
			orderID, shopName,
		)
	case "completed":
		msg = fmt.Sprintf(
			"🎉 *Order Completed!*\n\nYour order *#%s* has been marked as completed.\n\nThank you for shopping at %s! We hope to see you again soon. 😊",
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

	// Check order status by ID
	if strings.HasPrefix(messageLower, "track ") || strings.HasPrefix(messageLower, "status ") {
		parts := strings.Fields(message)
		if len(parts) == 2 {
			orderID := strings.ToUpper(parts[1])
			_, items, status, total, err := getOrderStatus(orderID)
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
			}
			return fmt.Sprintf("📦 *Order #%s*\n\nItems: %s\nTotal: ₦%.0f\nStatus: %s *%s*", orderID, items, total, statusEmoji, strings.ToUpper(status))
		}
	}

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
		sb.WriteString("To track an order, type *track ORDERID* e.g. track A3KX\n\nSend *hi* to place a new order.")
		return sb.String()
	}

	session, exists := sessions[from]
	if !exists || messageLower == "hi" || messageLower == "hello" || messageLower == "start" {
		// Check business hours
		if !isOpen() {
			return closedMessage()
		}
		sessions[from] = &Session{State: "GREETING"}
		session = sessions[from]
	}

	// Handle back navigation
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

		// Handle category selection
		if len(categories) > 1 {
			for i, cat := range categories {
				if messageLower == fmt.Sprintf("%d", i+1) || messageLower == strings.ToLower(cat) {
					session.BrowsingCategory = cat
					products, _ := getProductsByCategory(cat)
					return buildCatalogMessage(products)
				}
			}
		}

		// Handle product selection
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

		if session.BrowsingCategory != "" && (messageLower == "categories" || messageLower == "back to categories") {
			session.BrowsingCategory = ""
			return buildCategoryMessage(categories)
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
		return fmt.Sprintf("✅ *%s* removed from cart!\n\n%s", removed, buildBrowsingMessage(session))

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
					"🎉 *Order confirmed!*\n\nYour order ID is *%s*\n🚚 Delivery to: %s\n⏱️ Estimated time: *30 minutes*\n\n📌 *Please save your order ID* — type *track %s* anytime to check your order status.\n\nWe'll send you a message when your order is on the way!\n\nType *orders* to see your order history.",
					orderID, session.DeliveryAddress, orderID,
				)
			}
			return fmt.Sprintf(
				"🎉 *Order confirmed!*\n\nYour order ID is *%s*\nPickup time: *%s*\n\n📌 *Please save your order ID* — type *track %s* anytime to check your order status.\n\nWe'll send you a message when your order is ready for pickup!\n\nType *orders* to see your order history.",
				orderID, session.Pickup, orderID,
			)
		} else if messageLower == "no" || messageLower == "cancel" {
			delete(sessions, from)
			return "Order cancelled. Send *hi* to start again."
		}
		return "Please reply *yes* to confirm, *no* to cancel, or *back* to make changes."

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
		sb.WriteString(fmt.Sprintf("%d. %s — ₦%.0f\n", i+1, p.Name, p.Price))
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
		sb.WriteString(fmt.Sprintf("%d. %s x%d — ₦%.0f\n", i+1, item.Product.Name, item.Quantity, subtotal))
		total += subtotal
	}
	sb.WriteString(fmt.Sprintf("\n*Subtotal: ₦%.0f*", total))
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
		.stats { display: grid; grid-template-columns: repeat(2, 1fr); gap: 12px; margin-bottom: 16px; }
		@media(min-width: 600px) { .stats { grid-template-columns: repeat(4, 1fr); } }
		.stat { background: white; border-radius: 8px; padding: 14px; text-align: center; box-shadow: 0 1px 3px rgba(0,0,0,0.1); }
		.stat-value { font-size: 24px; font-weight: bold; color: #4CAF50; }
		.stat-label { font-size: 12px; color: #888; margin-top: 4px; }
		.search-bar { display: flex; gap: 10px; margin-bottom: 16px; }
		.search-bar input { flex: 1; padding: 10px; border: 1px solid #ddd; border-radius: 6px; font-size: 15px; }
		.form-row { display: flex; flex-direction: column; gap: 10px; }
		@media(min-width: 600px) { .form-row { flex-direction: row; } }
		input[type=text], input[type=number] { padding: 10px; border: 1px solid #ddd; border-radius: 6px; width: 100%%; font-size: 15px; }
		.btn { padding: 10px 18px; border: none; border-radius: 6px; cursor: pointer; font-size: 14px; font-weight: bold; width: 100%%; }
		@media(min-width: 600px) { .btn { width: auto; } }
		.btn-green { background: #4CAF50; color: white; }
		.btn-red { background: #f44336; color: white; }
		.btn-blue { background: #2196F3; color: white; }
		.btn-orange { background: #FF9800; color: white; }
		.btn-sm { padding: 6px 12px; font-size: 13px; width: auto; }
		.product-item { display: flex; justify-content: space-between; align-items: center; padding: 12px 0; border-bottom: 1px solid #eee; gap: 10px; flex-wrap: wrap; }
		.product-item:last-child { border-bottom: none; }
		.product-info { flex: 1; }
		.product-name { font-weight: bold; font-size: 15px; }
		.product-price { color: #4CAF50; font-size: 14px; margin-top: 2px; }
		.product-category { color: #888; font-size: 12px; margin-top: 2px; }
		.product-actions { display: flex; gap: 6px; flex-wrap: wrap; }
		.unavailable { opacity: 0.5; }
		.unavailable .product-name { text-decoration: line-through; }
		.order-card { border: 1px solid #eee; border-radius: 8px; padding: 14px; margin-bottom: 12px; background: white; }
		.order-header { display: flex; justify-content: space-between; align-items: center; margin-bottom: 8px; flex-wrap: wrap; gap: 6px; }
		.order-id { font-weight: bold; font-size: 16px; color: #333; }
		.badge { padding: 4px 10px; border-radius: 20px; font-size: 12px; font-weight: bold; }
		.badge-pending { background: #fff3cd; color: #856404; }
		.badge-ready { background: #d4edda; color: #155724; }
		.badge-out_for_delivery { background: #cce5ff; color: #004085; }
		.badge-completed { background: #e2e3e5; color: #383d41; }
		.order-details { font-size: 14px; color: #555; margin-bottom: 10px; line-height: 1.6; }
		.order-total { font-weight: bold; color: #333; }
		.delivery-badge { display: inline-block; padding: 2px 8px; border-radius: 10px; font-size: 11px; font-weight: bold; margin-left: 6px; }
		.delivery { background: #e3f2fd; color: #1565c0; }
		.pickup { background: #e8f5e9; color: #2e7d32; }
		.status-form { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
		select { padding: 8px; border: 1px solid #ddd; border-radius: 6px; font-size: 14px; flex: 1; }
		.empty { text-align: center; padding: 30px; color: #888; }
		.edit-price { display: flex; gap: 6px; align-items: center; }
		.edit-price input { width: 100px; padding: 6px; font-size: 13px; }
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
			category := r.FormValue("category")
			if category == "" {
				category = "General"
			}
			if name != "" && price > 0 {
				db.Exec(`INSERT INTO products (name, price, category) VALUES ($1, $2, $3)`, name, price, category)
			}
		case "delete_product":
			id := r.FormValue("id")
			db.Exec(`UPDATE products SET available = false WHERE id = $1`, id)
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
		<form method="POST" action="/admin">
			<input type="hidden" name="action" value="add_product">
			<div class="form-row">
				<input type="text" name="name" placeholder="Product name e.g. Rice (5kg)" required>
				<input type="number" name="price" placeholder="Price in ₦" min="1" required>
				<input type="text" name="category" placeholder="Category e.g. Grains">
				<button type="submit" class="btn btn-green">+ Add</button>
			</div>
		</form>
	</div>
	<div class="card">
		<h2>All Products (%d)</h2>`, len(products))

	if len(products) == 0 {
		fmt.Fprintf(w, `<div class="empty">No products yet. Add your first product above!</div>`)
	}

	currentCategory := ""
	for _, p := range products {
		if p.Category != currentCategory {
			if currentCategory != "" {
				fmt.Fprintf(w, `</div>`)
			}
			currentCategory = p.Category
			fmt.Fprintf(w, `<div style="margin-top:12px"><strong style="color:#4CAF50">📁 %s</strong></div><div>`, p.Category)
		}

		availableClass := ""
		toggleLabel := "Hide"
		toggleClass := "btn-orange"
		if !p.Available {
			availableClass = "unavailable"
			toggleLabel = "Show"
			toggleClass = "btn-green"
		}

		fmt.Fprintf(w, `
		<div class="product-item %s">
			<div class="product-info">
				<div class="product-name">%s</div>
				<div class="product-price">₦%.0f</div>
			</div>
			<div class="product-actions">
				<form method="POST" action="/admin" style="display:flex;gap:6px;align-items:center">
					<input type="hidden" name="action" value="edit_price">
					<input type="hidden" name="id" value="%d">
					<input type="number" name="price" placeholder="New price" min="1" style="width:100px;padding:6px;border:1px solid #ddd;border-radius:6px;font-size:13px">
					<button type="submit" class="btn btn-blue btn-sm">Update</button>
				</form>
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
		</div>`, availableClass, p.Name, p.Price, p.ID, p.ID, toggleClass, toggleLabel, p.ID)
	}

	fmt.Fprintf(w, `</div></div></div></body></html>`)
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

	// Search query
	search := r.URL.Query().Get("search")

	// Daily stats
	var totalOrders int
	var totalRevenue float64
	var pendingOrders int
	db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(total), 0) FROM orders WHERE created_at::date = CURRENT_DATE`).Scan(&totalOrders, &totalRevenue)
	db.QueryRow(`SELECT COUNT(*) FROM orders WHERE status = 'pending'`).Scan(&pendingOrders)

	// Fetch orders
	var rows *sql.Rows
	var err error
	if search != "" {
		rows, err = db.Query(
			`SELECT order_id, phone, items, total, delivery_type, delivery_address, pickup_slot, status, created_at FROM orders WHERE order_id ILIKE $1 OR phone ILIKE $1 ORDER BY created_at DESC`,
			"%"+search+"%",
		)
	} else {
		rows, err = db.Query(`SELECT order_id, phone, items, total, delivery_type, delivery_address, pickup_slot, status, created_at FROM orders ORDER BY created_at DESC`)
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

	// Stats
	fmt.Fprintf(w, `
	<div class="stats">
		<div class="stat"><div class="stat-value">%d</div><div class="stat-label">Today's Orders</div></div>
		<div class="stat"><div class="stat-value">₦%.0f</div><div class="stat-label">Today's Revenue</div></div>
		<div class="stat"><div class="stat-value">%d</div><div class="stat-label">Pending Orders</div></div>
		<div class="stat"><div class="stat-value">%d</div><div class="stat-label">Total Orders</div></div>
	</div>`, totalOrders, totalRevenue, pendingOrders, len(orders))

	// Search bar
	fmt.Fprintf(w, `
	<div class="search-bar">
		<form method="GET" action="/admin/orders" style="display:flex;gap:10px;flex:1">
			<input type="text" name="search" placeholder="Search by order ID or phone number..." value="%s">
			<button type="submit" class="btn btn-green" style="width:auto">Search</button>
			%s
		</form>
	</div>`, search, func() string {
		if search != "" {
			return `<a href="/admin/orders" class="btn btn-red" style="width:auto;padding:10px 18px;text-decoration:none;display:inline-block">Clear</a>`
		}
		return ""
	}())

	fmt.Fprintf(w, `<div class="card"><h2>Orders (%d)</h2></div>`, len(orders))

	if len(orders) == 0 {
		fmt.Fprintf(w, `<div class="empty">No orders found</div>`)
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
