package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

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
	State  string
	Cart   []CartItem
	Pickup string
}

// --- Globals ---

var sessions = map[string]*Session{}

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

	// Create orders table
	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS orders (
		id SERIAL PRIMARY KEY,
		order_id TEXT NOT NULL,
		phone TEXT NOT NULL,
		items TEXT NOT NULL,
		total REAL NOT NULL,
		pickup_slot TEXT NOT NULL,
		status TEXT DEFAULT 'pending',
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);`)
	if err != nil {
		log.Fatal("Failed to create orders table:", err)
	}

	// Create products table
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

	log.Println("✅ Database ready")
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

func saveOrder(phone string, cart []CartItem, pickup string, total float64) (string, error) {
	orderID := fmt.Sprintf("ORD-%d", time.Now().UnixMilli())

	var itemParts []string
	for _, item := range cart {
		itemParts = append(itemParts, fmt.Sprintf("%s x%d", item.Product.Name, item.Quantity))
	}
	itemsStr := strings.Join(itemParts, ", ")

	_, err := db.Exec(
		`INSERT INTO orders (order_id, phone, items, total, pickup_slot) VALUES ($1, $2, $3, $4, $5)`,
		orderID, phone, itemsStr, total, pickup,
	)
	if err != nil {
		return "", err
	}

	return orderID, nil
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

// --- Bot logic ---

func handleMessage(from, message string) string {
	message = strings.TrimSpace(strings.ToLower(message))

	session, exists := sessions[from]
	if !exists || message == "hi" || message == "hello" || message == "start" {
		sessions[from] = &Session{State: "GREETING"}
		session = sessions[from]
	}

	switch session.State {

	case "GREETING":
		session.State = "BROWSING"
		products, err := getProducts()
		if err != nil || len(products) == 0 {
			return "Sorry, our catalog is currently unavailable. Please try again later."
		}
		return buildCatalogMessage(products)

	case "BROWSING":
		products, _ := getProducts()
		for i, p := range products {
			if message == fmt.Sprintf("%d", i+1) {
				session.State = "QUANTITY"
				session.Cart = append(session.Cart, CartItem{Product: p, Quantity: 0})
				return fmt.Sprintf("How many of *%s* would you like? (Reply with a number)", p.Name)
			}
		}
		if message == "done" || message == "checkout" {
			if len(session.Cart) == 0 {
				return "Your cart is empty. Please select at least one item."
			}
			session.State = "PICKUP"
			return buildPickupMessage()
		}
		return "Please reply with a number from the menu, or type *done* to checkout."

	case "QUANTITY":
		qty := 0
		fmt.Sscanf(message, "%d", &qty)
		if qty <= 0 {
			return "Please enter a valid quantity (e.g. 1, 2, 3...)"
		}
		session.Cart[len(session.Cart)-1].Quantity = qty
		session.State = "BROWSING"
		products, _ := getProducts()
		return fmt.Sprintf("✅ Added! Reply with another number to add more items, or type *done* to checkout.\n\n%s", buildCatalogMessage(products))

	case "PICKUP":
		slot := 0
		fmt.Sscanf(message, "%d", &slot)
		if slot < 1 || slot > len(pickupSlots) {
			return "Please choose a valid slot number."
		}
		session.Pickup = pickupSlots[slot-1]
		session.State = "CONFIRM"
		return buildOrderSummary(session)

	case "CONFIRM":
		if message == "yes" || message == "confirm" {
			total := 0.0
			for _, item := range session.Cart {
				total += item.Product.Price * float64(item.Quantity)
			}

			orderID, err := saveOrder(from, session.Cart, session.Pickup, total)
			if err != nil {
				log.Printf("Error saving order: %v", err)
				return "Sorry, there was an error saving your order. Please try again."
			}

			session.State = "DONE"
			return fmt.Sprintf(
				"🎉 Order confirmed!\n\nYour order ID is *%s*\nPickup time: *%s*\n\nWe'll have your items ready. See you soon!",
				orderID, session.Pickup,
			)
		} else if message == "no" || message == "cancel" {
			delete(sessions, from)
			return "Order cancelled. Send *hi* to start again."
		}
		return "Please reply *yes* to confirm or *no* to cancel."

	case "DONE":
		return "Your order has been placed! Send *hi* to place a new order."
	}

	return "Send *hi* to start ordering."
}

// --- Message builders ---

func buildCatalogMessage(products []Product) string {
	var sb strings.Builder
	sb.WriteString("🛒 *Our Products:*\n\n")
	for i, p := range products {
		sb.WriteString(fmt.Sprintf("%d. %s — ₦%.0f\n", i+1, p.Name, p.Price))
	}
	sb.WriteString("\nReply with the *number* of the item you want.\nType *done* when you're ready to checkout.")
	return sb.String()
}

func buildPickupMessage() string {
	var sb strings.Builder
	sb.WriteString("🕐 *Choose a pickup time:*\n\n")
	for i, slot := range pickupSlots {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, slot))
	}
	sb.WriteString("\nReply with the slot number.")
	return sb.String()
}

func buildOrderSummary(s *Session) string {
	var sb strings.Builder
	sb.WriteString("📋 *Order Summary:*\n\n")
	total := 0.0
	for _, item := range s.Cart {
		subtotal := item.Product.Price * float64(item.Quantity)
		sb.WriteString(fmt.Sprintf("• %s x%d — ₦%.0f\n", item.Product.Name, item.Quantity, subtotal))
		total += subtotal
	}
	sb.WriteString(fmt.Sprintf("\n*Total: ₦%.0f*", total))
	sb.WriteString(fmt.Sprintf("\n📍 Pickup: %s", s.Pickup))
	sb.WriteString("\n\nReply *yes* to confirm or *no* to cancel.")
	return sb.String()
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

// --- Admin handler ---

func adminHandler(w http.ResponseWriter, r *http.Request) {
	// Handle product actions
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
		case "update_order_status":
			id := r.FormValue("id")
			status := r.FormValue("status")
			db.Exec(`UPDATE orders SET status = $1 WHERE order_id = $2`, status, id)
		}

		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}

	// Fetch products
	products, _ := getProducts()

	// Fetch orders
	rows, err := db.Query(`SELECT order_id, phone, items, total, pickup_slot, status, created_at FROM orders ORDER BY created_at DESC`)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type Order struct {
		OrderID    string
		Phone      string
		Items      string
		Total      float64
		PickupSlot string
		Status     string
		CreatedAt  string
	}

	var orders []Order
	for rows.Next() {
		var o Order
		rows.Scan(&o.OrderID, &o.Phone, &o.Items, &o.Total, &o.PickupSlot, &o.Status, &o.CreatedAt)
		orders = append(orders, o)
	}

	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head>
	<title>Supermarket Admin</title>
	<style>
		* { box-sizing: border-box; margin: 0; padding: 0; }
		body { font-family: Arial, sans-serif; background: #f5f5f5; }
		.header { background: #4CAF50; color: white; padding: 20px; }
		.header h1 { font-size: 24px; }
		.container { padding: 20px; max-width: 1100px; margin: auto; }
		.tabs { display: flex; gap: 10px; margin-bottom: 20px; margin-top: 20px; }
		.tab { padding: 10px 20px; background: white; border: 2px solid #4CAF50; border-radius: 6px; cursor: pointer; color: #4CAF50; font-weight: bold; text-decoration: none; }
		.tab.active { background: #4CAF50; color: white; }
		.card { background: white; border-radius: 8px; padding: 20px; margin-bottom: 20px; }
		.card h2 { margin-bottom: 15px; color: #333; }
		table { width: 100%%; border-collapse: collapse; }
		th { background: #4CAF50; color: white; padding: 10px; text-align: left; }
		td { padding: 10px; border-bottom: 1px solid #eee; }
		tr:hover { background: #f9f9f9; }
		.form-row { display: flex; gap: 10px; margin-bottom: 15px; }
		input[type=text], input[type=number] { padding: 10px; border: 1px solid #ddd; border-radius: 6px; flex: 1; font-size: 14px; }
		.btn { padding: 10px 20px; border: none; border-radius: 6px; cursor: pointer; font-size: 14px; font-weight: bold; }
		.btn-green { background: #4CAF50; color: white; }
		.btn-red { background: #f44336; color: white; }
		.btn-blue { background: #2196F3; color: white; }
		.pending { color: orange; font-weight: bold; }
		.ready { color: green; font-weight: bold; }
		select { padding: 8px; border: 1px solid #ddd; border-radius: 6px; }
	</style>
</head>
<body>
	<div class="header">
		<h1>🛒 Supermarket Admin Dashboard</h1>
	</div>
	<div class="container">
		<div class="tabs">
			<a href="/admin" class="tab active">📦 Products</a>
			<a href="/admin/orders" class="tab">🧾 Orders</a>
		</div>

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
			<h2>Current Products (%d)</h2>
			<table>
				<tr>
					<th>#</th>
					<th>Product Name</th>
					<th>Price</th>
					<th>Action</th>
				</tr>`, len(products))

	if len(products) == 0 {
		fmt.Fprintf(w, `<tr><td colspan="4" style="text-align:center;padding:20px">No products yet. Add your first product above!</td></tr>`)
	}

	for i, p := range products {
		fmt.Fprintf(w, `<tr>
			<td>%d</td>
			<td>%s</td>
			<td>₦%.0f</td>
			<td>
				<form method="POST" action="/admin" style="display:inline">
					<input type="hidden" name="action" value="delete_product">
					<input type="hidden" name="id" value="%d">
					<button type="submit" class="btn btn-red" onclick="return confirm('Delete this product?')">Delete</button>
				</form>
			</td>
		</tr>`, i+1, p.Name, p.Price, p.ID)
	}

	fmt.Fprintf(w, `</table></div></div></body></html>`)
}

// --- Admin orders handler ---

func adminOrdersHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		r.ParseForm()
		id := r.FormValue("id")
		status := r.FormValue("status")
		db.Exec(`UPDATE orders SET status = $1 WHERE order_id = $2`, status, id)
		http.Redirect(w, r, "/admin/orders", http.StatusSeeOther)
		return
	}

	rows, err := db.Query(`SELECT order_id, phone, items, total, pickup_slot, status, created_at FROM orders ORDER BY created_at DESC`)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type Order struct {
		OrderID    string
		Phone      string
		Items      string
		Total      float64
		PickupSlot string
		Status     string
		CreatedAt  string
	}

	var orders []Order
	for rows.Next() {
		var o Order
		rows.Scan(&o.OrderID, &o.Phone, &o.Items, &o.Total, &o.PickupSlot, &o.Status, &o.CreatedAt)
		orders = append(orders, o)
	}

	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head>
	<title>Supermarket Admin - Orders</title>
	<style>
		* { box-sizing: border-box; margin: 0; padding: 0; }
		body { font-family: Arial, sans-serif; background: #f5f5f5; }
		.header { background: #4CAF50; color: white; padding: 20px; }
		.header h1 { font-size: 24px; }
		.container { padding: 20px; max-width: 1100px; margin: auto; }
		.tabs { display: flex; gap: 10px; margin-bottom: 20px; margin-top: 20px; }
		.tab { padding: 10px 20px; background: white; border: 2px solid #4CAF50; border-radius: 6px; cursor: pointer; color: #4CAF50; font-weight: bold; text-decoration: none; }
		.tab.active { background: #4CAF50; color: white; }
		.card { background: white; border-radius: 8px; padding: 20px; margin-bottom: 20px; }
		.card h2 { margin-bottom: 15px; color: #333; }
		table { width: 100%%; border-collapse: collapse; }
		th { background: #4CAF50; color: white; padding: 10px; text-align: left; }
		td { padding: 10px; border-bottom: 1px solid #eee; vertical-align: middle; }
		tr:hover { background: #f9f9f9; }
		.btn { padding: 8px 14px; border: none; border-radius: 6px; cursor: pointer; font-size: 13px; font-weight: bold; }
		.btn-green { background: #4CAF50; color: white; }
		.pending { color: orange; font-weight: bold; }
		.ready { color: green; font-weight: bold; }
		select { padding: 8px; border: 1px solid #ddd; border-radius: 6px; }
	</style>
</head>
<body>
	<div class="header">
		<h1>🛒 Supermarket Admin Dashboard</h1>
	</div>
	<div class="container">
		<div class="tabs">
			<a href="/admin" class="tab">📦 Products</a>
			<a href="/admin/orders" class="tab active">🧾 Orders</a>
		</div>
		<div class="card">
			<h2>All Orders (%d)</h2>
			<table>
				<tr>
					<th>Order ID</th>
					<th>Phone</th>
					<th>Items</th>
					<th>Total</th>
					<th>Pickup</th>
					<th>Status</th>
					<th>Time</th>
					<th>Action</th>
				</tr>`, len(orders))

	if len(orders) == 0 {
		fmt.Fprintf(w, `<tr><td colspan="8" style="text-align:center;padding:20px">No orders yet</td></tr>`)
	}

	for _, o := range orders {
		fmt.Fprintf(w, `<tr>
			<td>%s</td>
			<td>%s</td>
			<td>%s</td>
			<td>₦%.0f</td>
			<td>%s</td>
			<td class="%s">%s</td>
			<td>%s</td>
			<td>
				<form method="POST" action="/admin/orders" style="display:inline">
					<input type="hidden" name="id" value="%s">
					<select name="status">
						<option value="pending">Pending</option>
						<option value="ready">Ready</option>
						<option value="completed">Completed</option>
					</select>
					<button type="submit" class="btn btn-green">Update</button>
				</form>
			</td>
		</tr>`, o.OrderID, o.Phone, o.Items, o.Total, o.PickupSlot, o.Status, o.Status, o.CreatedAt, o.OrderID)
	}

	fmt.Fprintf(w, `</table></div></div></body></html>`)
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
	http.HandleFunc("/admin", adminHandler)
	http.HandleFunc("/admin/orders", adminOrdersHandler)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})

	addr := "0.0.0.0:" + port
	log.Printf("🚀 Bot server running on %s", addr)
	log.Printf("📊 Admin dashboard at http://localhost:%s/admin", port)
	log.Fatal(http.ListenAndServe(addr, nil))
}
