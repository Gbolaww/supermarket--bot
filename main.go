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

	_ "github.com/mattn/go-sqlite3"
)

// --- Data Models ---

type Product struct {
	ID    string
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

var catalog = []Product{
	{ID: "1", Name: "Rice (5kg)", Price: 4500},
	{ID: "2", Name: "Tomatoes (basket)", Price: 2000},
	{ID: "3", Name: "Chicken (1kg)", Price: 3500},
	{ID: "4", Name: "Bread (loaf)", Price: 800},
	{ID: "5", Name: "Eggs (crate)", Price: 2500},
}

var pickupSlots = []string{
	"10:00 AM - 11:00 AM",
	"12:00 PM - 1:00 PM",
	"3:00 PM - 4:00 PM",
	"5:00 PM - 6:00 PM",
}

var db *sql.DB

// --- Database setup ---

func initDB() {
	var err error
	db, err = sql.Open("sqlite3", "./orders.db")
	if err != nil {
		log.Fatal("Failed to open database:", err)
	}

	createTable := `
	CREATE TABLE IF NOT EXISTS orders (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		order_id TEXT NOT NULL,
		phone TEXT NOT NULL,
		items TEXT NOT NULL,
		total REAL NOT NULL,
		pickup_slot TEXT NOT NULL,
		status TEXT DEFAULT 'pending',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);`

	_, err = db.Exec(createTable)
	if err != nil {
		log.Fatal("Failed to create table:", err)
	}

	log.Println("✅ Database ready")
}

func saveOrder(phone string, cart []CartItem, pickup string, total float64) (string, error) {
	orderID := fmt.Sprintf("ORD-%d", time.Now().UnixMilli())

	// Build items summary string
	var itemParts []string
	for _, item := range cart {
		itemParts = append(itemParts, fmt.Sprintf("%s x%d", item.Product.Name, item.Quantity))
	}
	itemsStr := strings.Join(itemParts, ", ")

	_, err := db.Exec(
		`INSERT INTO orders (order_id, phone, items, total, pickup_slot) VALUES (?, ?, ?, ?, ?)`,
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
		return buildCatalogMessage()

	case "BROWSING":
		for i, p := range catalog {
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
		return fmt.Sprintf("✅ Added! Reply with another number to add more items, or type *done* to checkout.\n\n%s", buildCatalogMessage())

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
			// Calculate total
			total := 0.0
			for _, item := range session.Cart {
				total += item.Product.Price * float64(item.Quantity)
			}

			// Save to database
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

func buildCatalogMessage() string {
	var sb strings.Builder
	sb.WriteString("🛒 *Our Products:*\n\n")
	for i, p := range catalog {
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
	<title>Supermarket Orders</title>
	<style>
		body { font-family: Arial, sans-serif; padding: 20px; background: #f5f5f5; }
		h1 { color: #333; }
		table { width: 100%%; border-collapse: collapse; background: white; border-radius: 8px; overflow: hidden; }
		th { background: #4CAF50; color: white; padding: 12px; text-align: left; }
		td { padding: 12px; border-bottom: 1px solid #eee; }
		tr:hover { background: #f9f9f9; }
		.pending { color: orange; font-weight: bold; }
		.confirmed { color: green; font-weight: bold; }
	</style>
</head>
<body>
	<h1>🛒 Supermarket Orders</h1>
	<table>
		<tr>
			<th>Order ID</th>
			<th>Phone</th>
			<th>Items</th>
			<th>Total</th>
			<th>Pickup Slot</th>
			<th>Status</th>
			<th>Time</th>
		</tr>`)

	if len(orders) == 0 {
		fmt.Fprintf(w, `<tr><td colspan="7" style="text-align:center">No orders yet</td></tr>`)
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
		</tr>`, o.OrderID, o.Phone, o.Items, o.Total, o.PickupSlot, o.Status, o.Status, o.CreatedAt)
	}

	fmt.Fprintf(w, `</table></body></html>`)
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
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})

	addr := "0.0.0.0:" + port
	log.Printf("🚀 Bot server running on %s", addr)
	log.Printf("📊 Admin dashboard at http://localhost:%s/admin", port)
	log.Fatal(http.ListenAndServe(addr, nil))
}
