package main

import (
	"context"
	"fmt"
	"html"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
	"github.com/spf13/cast"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"

	"gopkg.in/telebot.v3"
)

const (
	readRange = "Лист1!A2:AZ"
	sheetName = "Лист1"
)

var spreadsheetID = ""

type userState struct {
	Step       int
	Name       string
	Phone      string
	Summa      string
	NominalSum string
}

type editState struct {
	Step  int
	Name  string // will contain "Name|Phone"
	Year  string
	Month string
}

type Storage struct {
	Data [][]any
	mu   sync.Mutex
}

var (
	userStates   = make(map[int64]*userState)
	editStates   = make(map[int64]*editState)
	allowedUsers = map[string]bool{}

	srv *sheets.Service
)

var monthsMap = map[string]int{
	"Yanvar":  0,
	"Fevral":  1,
	"Mart":    2,
	"Aprel":   3,
	"May":     4,
	"Iyun":    5,
	"Iyul":    6,
	"Avgust":  7,
	"Sentabr": 8,
	"Oktabr":  9,
	"Noyabr":  10,
	"Dekabr":  11,
}

type Config struct {
	SpreadsheetID string   `env:"spreadsheetID"`
	TelegramToken string   `env:"telegramToken"`
	AllowedUsers  []string `env:"allowedUsers" envSeparator:","`
}

func main() {
	var cfg Config

	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found")
	}

	if err := env.Parse(&cfg); err != nil {
		log.Fatalf("Failed to parse env vars: %v", err)
	}

	fmt.Printf("Config: %+v\n", cfg)

	spreadsheetID = cfg.SpreadsheetID
	if spreadsheetID == "" {
		log.Fatal("spreadsheetID is required")
	}

	if cfg.TelegramToken == "" {
		log.Fatal("telegramToken is required")
	}

	for _, u := range cfg.AllowedUsers {
		allowedUsers[strings.ToLower(u)] = true
	}

	var (
		strg = &Storage{
			Data: [][]any{},
			mu:   sync.Mutex{},
		}
		pref = telebot.Settings{
			Token:  cfg.TelegramToken,
			Poller: &telebot.LongPoller{Timeout: 10 * time.Second},
		}
	)

	bot, err := telebot.NewBot(pref)
	if err != nil {
		log.Fatal(err)
	}

	var ctx = context.Background()

	b, err := os.ReadFile("credentials.json")
	if err != nil {
		log.Fatalf("Unable to read client secret file: %v", err)
	}

	config, err := google.JWTConfigFromJSON(b, sheets.SpreadsheetsScope)
	if err != nil {
		log.Fatalf("Unable to parse JWT config: %v", err)
	}

	var client = config.Client(ctx)

	srv, err = sheets.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		log.Fatalf("Unable to retrieve Sheets client: %v", err)
	}

	refreshData(strg)

	bot.Use(authMiddleware)

	// Handle /start command
	bot.Handle("/start", func(c telebot.Context) error {
		msg := `👋 Xush kelibsiz!
				Quyidagi buyruqlardan foydalanishingiz mumkin:
				- /list   -- Shaxslar ro'yxatini ko'rish
				- /add    -- Yangi shaxs qo'shish
				- /edit   -- Mavjud shaxs ma'lumotlarini o'zgartirish
				- /totals -- Umumiy natijalarni ko'rish
		`

		return c.Send(msg)
	})

	// Handle /list command
	bot.Handle("/list", func(c telebot.Context) error {
		refreshData(strg)
		menu := &telebot.ReplyMarkup{}
		var rows []telebot.Row

		for indx, row := range strg.Data {
			if indx == 0 {
				continue // skip header
			}
			if len(row) > 1 {
				name := fmt.Sprintf("%v", row[0])
				phone := fmt.Sprintf("%v", row[1])
				label := fmt.Sprintf("%s (%s)", name, phone)
				btn := menu.Data(label, "select", name+"|"+phone)

				// each row = one button
				rows = append(rows, menu.Row(btn))
			}
		}

		menu.Inline(rows...)
		return c.Send("Shaxsni tanlang:", menu)
	})

	// Handle /add command
	bot.Handle("/add", func(c telebot.Context) error {
		userStates[c.Sender().ID] = &userState{Step: 1}
		return c.Send("✏️ Ism kiriting:(Masalan, Abdusattor yoki Sardor)")
	})

	// Handle /edit command
	bot.Handle("/edit", func(c telebot.Context) error {
		refreshData(strg)
		menu := &telebot.ReplyMarkup{}
		var buttons []telebot.Btn
		for _, row := range strg.Data {
			if len(row) > 1 {
				name := fmt.Sprintf("%v", row[0])
				phone := fmt.Sprintf("%v", row[1])
				label := fmt.Sprintf("%s (%s)", name, phone)
				btn := menu.Data(label, "edit_name", name+"|"+phone)
				buttons = append(buttons, btn)
			}
		}
		menu.Inline(buttons)
		return c.Send("✏️ O'zgartirish uchun shaxsni tanlang:", menu)
	})

	// Handle /totals command
	bot.Handle("/totals", func(c telebot.Context) error {
		refreshData(strg)

		var (
			totalSumma      float64
			totalBerdi      float64
			totalQoldiq     float64
			totalNominalSum float64
		)

		for _, row := range strg.Data {
			if len(row) < 6 {
				continue
			}

			totalSumma += parseFloat(row[2])      // C (Summa)
			totalBerdi += parseFloat(row[3])      // D (Berdi)
			totalQoldiq += parseFloat(row[4])     // E (Qoldiq)
			totalNominalSum += parseFloat(row[5]) // F (Nominal Sum)
		}

		msg := fmt.Sprintf(
			"<pre> 📊 Umumiy natijalar:\n\n💰 Summa: %s\n✅ Berdi: %s\n💸 Qoldiq: %s\n📊 Nominal Sum: %s </pre>",
			formatMoney(totalSumma),
			formatMoney(totalBerdi),
			formatMoney(totalQoldiq),
			formatMoney(totalNominalSum),
		)

		return c.Send(msg, telebot.ModeHTML)
	})

	// Handle add + edit text flow
	bot.Handle(telebot.OnText, func(c telebot.Context) error {
		// ADD FLOW
		if state, ok := userStates[c.Sender().ID]; ok {
			switch state.Step {
			case 1:
				state.Name = c.Text()
				state.Step = 2
				return c.Send("📱 Telefon kiriting: (Masalan, iPhone 16 Pro Max modelini yozing)")
			case 2:
				state.Phone = formatPhoneModel(c.Text())
				state.Step = 3
				return c.Send("💰 Summani kiriting: (Bu yerga bergan summangizni $ belgisisiz, faqat raqam yozing)")
			case 3:
				state.Summa = c.Text()
				state.Step = 4
				return c.Send("📊 Nominal summani kiriting: (Telefonning haqiqiy narxini $ belgisisiz, faqat raqam yozing)")
			case 4:
				state.NominalSum = c.Text()

				rowIndex := len(strg.Data) + 2
				row := []any{
					state.Name,
					state.Phone,
					state.Summa,
					fmt.Sprintf("=СУММ(G%d:AZ%d)", rowIndex, rowIndex),
					fmt.Sprintf("=C%d-D%d", rowIndex, rowIndex),
					state.NominalSum,
				}

				vr := &sheets.ValueRange{Values: [][]any{row}}
				_, err := srv.Spreadsheets.
					Values.
					Append(spreadsheetID, sheetName+"!A3", vr).
					ValueInputOption("USER_ENTERED").
					Do()
				if err != nil {
					return c.Send("❌ Failed to save: " + err.Error())
				}

				delete(userStates, c.Sender().ID)
				refreshData(strg)

				return c.Send(fmt.Sprintf(`✅ Qo'shildi: %s, %s, %s, Nominal: %s`, state.Name, state.Phone, state.Summa, state.NominalSum))
			}
		}

		// EDIT FLOW (enter summa)
		if state, ok := editStates[c.Sender().ID]; ok && state.Step == 3 {
			summa := c.Text()

			parts := strings.SplitN(state.Name, "|", 2)
			if len(parts) != 2 {
				return c.Send("❌ Internal error: invalid state")
			}
			targetName, targetPhone := parts[0], parts[1]

			var rowIndex int
			for i, row := range strg.Data {
				if len(row) > 1 &&
					strings.EqualFold(fmt.Sprintf("%v", row[0]), targetName) &&
					strings.EqualFold(fmt.Sprintf("%v", row[1]), targetPhone) {
					rowIndex = i + 2
					break
				}
			}

			var col int
			switch state.Year {
			case "2025":
				col = 7 + monthsMap[state.Month]
			case "2026":
				col = 19 + monthsMap[state.Month]
			}

			cell := fmt.Sprintf("%s%d", string(rune('A'+col-1)), rowIndex)

			vr := &sheets.ValueRange{Values: [][]any{{summa}}}
			_, err := srv.Spreadsheets.Values.Update(spreadsheetID, sheetName+"!"+cell, vr).
				ValueInputOption("USER_ENTERED").Do()
			if err != nil {
				return c.Send("❌ Failed to update: " + err.Error())
			}

			delete(editStates, c.Sender().ID)
			refreshData(strg)
			return c.Send(fmt.Sprintf("✅ %s-yil %s oyi yangilandi — %s (%s) = %s", state.Year, state.Month, targetName, targetPhone, summa))
		}

		return nil
	})

	// Handle select list flow
	bot.Handle(&telebot.Btn{Unique: "select"}, func(c telebot.Context) error {
		parts := strings.SplitN(c.Data(), "|", 2)
		if len(parts) != 2 {
			return c.Send("❌ Invalid selection")
		}
		selectedName, selectedPhone := parts[0], parts[1]

		var result string
		months := []string{"Yanvar", "Fevral", "Mart", "Aprel", "May", "Iyun", "Iyul", "Avgust", "Sentabr", "Oktabr", "Noyabr", "Dekabr"}

		for _, row := range strg.Data {
			if len(row) > 1 &&
				strings.EqualFold(fmt.Sprintf("%v", row[0]), selectedName) &&
				strings.EqualFold(fmt.Sprintf("%v", row[1]), selectedPhone) {

				result = fmt.Sprintf("📌 Name: %v\n📱 Phone: %v\n💰 Summa: %v\n✅ Berdi: %v\n💸 Qoldiq: %v\n📊 Nominal Sum: %v\n🤑 Foyda: %v\n\n",
					row[0], row[1],
					formatMoney(parseFloat(row[2])),
					formatMoney(parseFloatFromCell(row[3])),
					formatMoney(parseFloatFromCell(row[4])),
					formatMoney(parseFloat(row[5])),
					formatMoney(parseFloat(fmt.Sprintf("%v", row[2]))-parseFloat(fmt.Sprintf("%v", row[5]))),
				)

				result += "📅 Payments:\n<pre>"

				result += "--------------2025--------------\n"
				for i, m := range months {
					col := 6 + i
					val := 0.0
					if col < len(row) {
						val = parseFloatFromCell(row[col])
					}
					line := fmt.Sprintf("📅 %-9s: %9s\n", m, formatMoney(val))
					result += html.EscapeString(line)
				}

				result += "--------------2026--------------\n"
				for i, m := range months {
					col := 18 + i
					val := 0.0
					if col < len(row) {
						val = parseFloatFromCell(row[col])
					}
					line := fmt.Sprintf("📅 %-9s: %9s\n", m, formatMoney(val))
					result += html.EscapeString(line)
				}

				result += "</pre>"
				break
			}
		}

		if result == "" {
			result = "Not found."
		}

		return c.Send(result, telebot.ModeHTML)
	})

	// Handle edit flow: select name
	bot.Handle(&telebot.Btn{Unique: "edit_name"}, func(c telebot.Context) error {
		editStates[c.Sender().ID] = &editState{Step: 1, Name: c.Data()}
		menu := &telebot.ReplyMarkup{}
		years := []string{"2025", "2026"}
		var buttons []telebot.Btn
		for _, y := range years {
			btn := menu.Data(y, "edit_year", y)
			buttons = append(buttons, btn)
		}
		menu.Inline(buttons)
		return c.Send("📅 Select year:", menu)
	})

	// Handle edit flow: select year
	bot.Handle(&telebot.Btn{Unique: "edit_year"}, func(c telebot.Context) error {
		state := editStates[c.Sender().ID]
		state.Year = c.Data()
		state.Step = 2

		menu := &telebot.ReplyMarkup{}
		monthsOrder := []string{"Yanvar", "Fevral", "Mart", "Aprel", "May", "Iyun", "Iyul", "Avgust", "Sentabr", "Oktabr", "Noyabr", "Dekabr"}

		var rows []telebot.Row
		for i := 0; i < len(monthsOrder); i += 3 {
			var btns []telebot.Btn
			for j := i; j < i+3 && j < len(monthsOrder); j++ {
				btns = append(btns, menu.Data(monthsOrder[j], "edit_month", monthsOrder[j]))
			}
			rows = append(rows, menu.Row(btns...))
		}
		menu.Inline(rows...)
		return c.Send("📅 Oyni tanlang:", menu)
	})

	// Handle edit flow: select month
	bot.Handle(&telebot.Btn{Unique: "edit_month"}, func(c telebot.Context) error {
		state := editStates[c.Sender().ID]
		state.Month = c.Data()
		state.Step = 3
		return c.Send(fmt.Sprintf("💰 Enter summa for %s %s (%s):", state.Month, state.Year, state.Name))
	})

	log.Println("Bot started...")
	bot.Start()
}

func refreshData(data *Storage) {
	data.mu.Lock()
	defer data.mu.Unlock()

	resp, err := srv.Spreadsheets.Values.Get(spreadsheetID, readRange).Do()
	if err != nil {
		log.Fatalf("Unable to retrieve data from sheet: %v", err)
	}

	data.Data = resp.Values
}

func authMiddleware(next telebot.HandlerFunc) telebot.HandlerFunc {
	return func(c telebot.Context) error {
		u := strings.ToLower(c.Sender().Username)
		if u == "" || !allowedUsers[u] {
			return c.Send("❌ You are not allowed to use this bot.")
		}
		return next(c)
	}
}

func parseFloatFromCell(v any) float64 {
	if v == nil {
		return 0
	}

	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0
		}
		s = strings.ReplaceAll(s, "$", "")
		s = strings.ReplaceAll(s, " ", "")
		s = strings.ReplaceAll(s, ",", ".")
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0
		}
		return f
	default:
		s := fmt.Sprintf("%v", v)
		s = strings.TrimSpace(s)
		s = strings.ReplaceAll(s, "$", "")
		s = strings.ReplaceAll(s, " ", "")
		s = strings.ReplaceAll(s, ",", ".")
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0
		}
		return f
	}
}

func formatMoney(val float64) string {
	s := fmt.Sprintf("%.2f", val)
	s = strings.ReplaceAll(s, ".", ",")
	return "$" + s
}

func parseFloat(val any) float64 {
	s := fmt.Sprintf("%v", val)
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, s)

	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, ",", ".")
	return cast.ToFloat64(s)
}

func formatPhoneModel(phone string) string {
	phone = strings.TrimSpace(phone)

	phone = strings.ReplaceAll(phone, "  ", " ")

	lower := strings.ToLower(phone)

	replacements := map[string]string{
		"iphone":      "iPhone",
		"iphone14":    "iPhone 14",
		"iphone13":    "iPhone 13",
		"iphone12":    "iPhone 12",
		"iphone11":    "iPhone 11",
		"iphone x":    "iPhone X",
		"iphone8":     "iPhone 8",
		"iphone7":     "iPhone 7",
		"iphone6":     "iPhone 6",
		"ipad":        "iPad",
		"airpods":     "AirPods",
		"airpod":      "AirPods",
		"airpod2":     "AirPods 2",
		"airpod3":     "AirPods 3",
		"airpod4":     "AirPods 4",
		"airpod pro":  "AirPods Pro",
		"airpods pro": "AirPods Pro",
		"airpodspro":  "AirPods Pro",
		"IPhone":      "iPhone",
		"Iphone":      "iPhone",
		"promax":      "Pro Max",
		"pro max":     "Pro Max",
		"pro  max":    "Pro Max",
		"pro":         "Pro",
		"max":         "Max",
		"plus":        "Plus",
		"mini":        "Mini",
		"ultra":       "Ultra",
		"galaxy":      "Galaxy",
		"samsung":     "Samsung",
		"xiaomi":      "Xiaomi",
		"redmi":       "Redmi",
		"note":        "Note",
	}

	for k, v := range replacements {
		lower = strings.ReplaceAll(lower, k, v)
	}
	lower = strings.ReplaceAll(lower, "Pro Pro", "Pro")

	lower = strings.ReplaceAll(lower, "Max Max", "Max")
	lower = strings.ReplaceAll(lower, "Pro Max Max", "Pro Max")
	lower = strings.ReplaceAll(lower, "  ", " ")

	words := strings.Fields(lower)
	for i := range words {
		if len(words[i]) > 0 {
			words[i] = strings.ToUpper(words[i][:1]) + words[i][1:]
		}
	}
	return strings.Join(words, " ")
}
