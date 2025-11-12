package main

import (
	"context"
	"fmt"
	"html"
	"log"
	"os"
	"reflect"
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

const readRange = "Лист1!A3:AZ"

type Storage struct {
	Data [][]any
	mu   sync.Mutex
}

var (
	allowedUsers  = map[string]bool{}
	srv           *sheets.Service
	spreadsheetID = ""
)

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

	{
		if spreadsheetID == "" {
			log.Fatal("spreadsheetID is required")
		}

		if cfg.TelegramToken == "" {
			log.Fatal("telegramToken is required")
		}

		for _, u := range cfg.AllowedUsers {
			allowedUsers[strings.ToLower(u)] = true
		}
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
	bot.Handle("/start",
		func(c telebot.Context) error {
			var msg = `👋 Xush kelibsiz!
				Quyidagi buyruqlardan foydalanishingiz mumkin:
				- /list    -- Shaxslar ro'yxatini ko'rish
				- /totals  -- Umumiy natijalarni ko'rish
				- /bymonth -- Oy bo'yicha to'lovlarni ko'rish
			`

			return c.Send(msg)
		},
	)

	// Handle /list command
	bot.Handle("/list",
		func(c telebot.Context) error {
			refreshData(strg)

			var (
				menu = &telebot.ReplyMarkup{}
				rows []telebot.Row
			)

			{
				if len(strg.Data) == 0 {
					return c.Send("❌ No data found in spreadsheet")
				}
			}

			for _, row := range strg.Data {
				if len(row) > 1 {
					var (
						name  = fmt.Sprintf("%v", row[0])
						phone = fmt.Sprintf("%v", row[1])
					)

					if strings.TrimSpace(name) == "" || strings.TrimSpace(phone) == "" {
						continue
					}

					var (
						label = fmt.Sprintf("%s (%s)", name, phone)
						btn   = menu.Data(label, "select", name+"|"+phone)
					)

					if parseFloatFromCell(row[4]) == 0 {
						continue
					}

					rows = append(rows, menu.Row(btn))
				}
			}

			if len(rows) == 0 {
				return c.Send("❌ No valid data found")
			}

			menu.Inline(rows...)
			return c.Send("Shaxsni tanlang:", menu)
		},
	)

	bot.Handle("/totals",
		func(c telebot.Context) error {
			refreshData(strg)

			if len(strg.Data) == 0 {
				return c.Send("❌ No data found in spreadsheet")
			}

			var (
				totalSumma      float64
				totalBerdi      float64
				totalQoldiq     float64
				totalOyega      float64
				totalNominalSum float64
			)

			for _, row := range strg.Data {
				if len(row) < 6 {
					continue
				}

				// Skip rows with empty names
				if len(row) > 0 && strings.TrimSpace(fmt.Sprintf("%v", row[0])) == "" {
					continue
				}

				if parseFloatFromCell(row[4]) == 0 {
					continue
				}

				totalSumma += parseFloat(row[2])          // C (Summa)
				totalBerdi += parseFloatFromCell(row[3])  // D (Berdi) - formula field
				totalQoldiq += parseFloatFromCell(row[4]) // E (Qoldiq) - formula field
				totalNominalSum += parseFloat(row[5])     // F (Nominal Sum)
				totalOyega += parseFloatFromCell(row[8])  // I (Oyega) - formula field
			}

			msg := fmt.Sprintf(
				`<pre> 
📊 Umumiy natijalar:
💰 Summa: %s
✅ Berdi: %s
💸 Qoldiq: %s
📊 Nominal Sum: %s
🎰 Oyega: %s
🤑 Foyda: %s </pre>`,
				formatMoney(totalSumma),
				formatMoney(totalBerdi),
				formatMoney(totalQoldiq),
				formatMoney(totalNominalSum),
				formatMoney(totalOyega),
				formatMoney(totalSumma-totalNominalSum),
			)

			return c.Send(msg, telebot.ModeHTML)
		},
	)

	// Handle /bymonth command
	bot.Handle("/bymonth",
		func(c telebot.Context) error {
			menu := &telebot.ReplyMarkup{}
			years := []string{"2025", "2026"}
			var buttons []telebot.Btn
			for _, y := range years {
				btn := menu.Data(y, "bymonth_year", y)
				buttons = append(buttons, btn)
			}
			menu.Inline(buttons)
			return c.Send("📅 Yilni tanlang:", menu)
		},
	)

	// Handle select list flow
	bot.Handle(&telebot.Btn{Unique: "select"},
		func(c telebot.Context) error {
			refreshData(strg)

			parts := strings.SplitN(c.Data(), "|", 2)
			if len(parts) != 2 {
				return c.Send("❌ Invalid selection")
			}

			var (
				selectedName, selectedPhone = parts[0], parts[1]
				months                      = []string{"Yanvar", "Fevral", "Mart", "Aprel", "May", "Iyun", "Iyul", "Avgust", "Sentabr", "Oktabr", "Noyabr", "Dekabr"}
				result                      string
			)

			for _, row := range strg.Data {
				if len(row) > 1 &&
					strings.EqualFold(fmt.Sprintf("%v", row[0]), selectedName) &&
					strings.EqualFold(fmt.Sprintf("%v", row[1]), selectedPhone) {

					// Ensure we have enough columns
					var (
						summa      = 0.0
						berdi      = 0.0
						qoldiq     = 0.0
						nominalSum = 0.0
					)

					if len(row) > 2 {
						summa = parseFloat(row[2])
					}

					if len(row) > 3 {
						berdi = parseFloatFromCell(row[3])
					}
					if len(row) > 4 {
						qoldiq = parseFloatFromCell(row[4])
					}
					if len(row) > 5 {
						nominalSum = parseFloat(row[5])
					}

					result = fmt.Sprintf(
						`<b>📌 Name:</b> %v
<b>📱 Phone:</b> %v

<b>💰 Summa:</b> %v
<b>✅ Berdi:</b> %v
<b>💸 Qoldiq:</b> %v
<b>📊 Nominal Sum:</b> %v
<b>🤑 Foyda:</b> %v

<b>📅 Sana:</b> %v
<b>📆 Oy:</b> %v
<b>🎰 Oyega:</b> %v
`,
						row[0], row[1],
						formatMoney(summa),
						formatMoney(berdi),
						formatMoney(qoldiq),
						formatMoney(nominalSum),
						formatMoney(summa-nominalSum),
						row[6],
						row[7],
						row[8],
					)

					result += "🛒 Payments:\n<pre>"

					result += "--------------2025--------------\n"
					for i, m := range months {
						var (
							col = 9 + i // G=6, H=7, ..., R=17 (0-indexed)
							val = 0.0
						)

						if col < len(row) {
							val = parseFloatFromCell(row[col])
						}

						var line = fmt.Sprintf("📅 %-9s: %9s\n", m, formatMoney(val))

						result += html.EscapeString(line)
					}

					result += "--------------2026--------------\n"
					for i, m := range months {
						var (
							col = 22 + i // S=18, T=19, ..., AD=29 (0-indexed)
							val = 0.0
						)

						if col < len(row) {
							val = parseFloatFromCell(row[col])
						}

						var line = fmt.Sprintf("📅 %-9s: %9s\n", m, formatMoney(val))

						result += html.EscapeString(line)
					}

					result += "</pre>"
					break
				}
			}

			if result == "" {
				result = "❌ Person not found."
			}

			return c.Send(result, telebot.ModeHTML)
		},
	)

	// Handle bymonth year selection
	bot.Handle(&telebot.Btn{Unique: "bymonth_year"},
		func(c telebot.Context) error {
			year := c.Data()
			menu := &telebot.ReplyMarkup{}
			months := []string{"Yanvar", "Fevral", "Mart", "Aprel", "May", "Iyun", "Iyul", "Avgust", "Sentabr", "Oktabr", "Noyabr", "Dekabr"}

			var rows []telebot.Row
			for i := 0; i < len(months); i += 3 {
				var btns []telebot.Btn
				for j := i; j < i+3 && j < len(months); j++ {
					btns = append(btns, menu.Data(months[j], "bymonth_month", year+"|"+months[j]))
				}
				rows = append(rows, menu.Row(btns...))
			}
			menu.Inline(rows...)
			return c.Send("📅 Oyni tanlang:", menu)
		},
	)

	// Handle bymonth month selection
	bot.Handle(&telebot.Btn{Unique: "bymonth_month"},
		func(c telebot.Context) error {
			refreshData(strg)

			parts := strings.SplitN(c.Data(), "|", 2)
			if len(parts) != 2 {
				return c.Send("❌ Invalid selection")
			}

			year := parts[0]
			month := parts[1]

			// Map month names to column indices
			monthsMap := map[string]int{
				"Yanvar": 0, "Fevral": 1, "Mart": 2, "Aprel": 3,
				"May": 4, "Iyun": 5, "Iyul": 6, "Avgust": 7,
				"Sentabr": 8, "Oktabr": 9, "Noyabr": 10, "Dekabr": 11,
			}

			monthIndex, ok := monthsMap[month]
			if !ok {
				return c.Send("❌ Invalid month")
			}

			var col int
			switch year {
			case "2025":
				col = 9 + monthIndex // Columns J-U (index 9-20)
			case "2026":
				col = 22 + monthIndex // Columns W-AH (index 22-33)
			default:
				return c.Send("❌ Invalid year")
			}

			if len(strg.Data) == 0 {
				return c.Send("❌ No data found in spreadsheet")
			}

			var result string
			var totalPayment float64
			var hasData bool
			var count = 1

			for _, row := range strg.Data {
				if len(row) < 2 || strings.TrimSpace(fmt.Sprintf("%v", row[0])) == "" {
					continue
				}

				var payment float64
				if len(row) > col {
					payment = parseFloatFromCell(row[col])
				}

				name := fmt.Sprintf("%v", row[0])
				result += fmt.Sprintf("%d. %s — 💰 %s\n", count, html.EscapeString(name), formatMoney(payment))

				totalPayment += payment
				hasData = true
				count++
			}

			if !hasData {
				return c.Send(fmt.Sprintf("❌ %s %s oyida hech qanday to'lov topilmadi", month, year))
			}

			result = fmt.Sprintf("📅 <b>%s %s</b>\n\n%s\n<b>Jami:</b> %s", month, year, result, formatMoney(totalPayment))

			return c.Send(result, telebot.ModeHTML)
		},
	)

	log.Println("Bot started...")
	bot.Start()
}

func refreshData(data *Storage) {
	data.mu.Lock()
	defer data.mu.Unlock()

	resp, err := srv.Spreadsheets.Values.Get(spreadsheetID, readRange).Do()
	if err != nil {
		log.Printf("Error retrieving data from sheet: %v", err)
		return
	}

	data.Data = resp.Values
	log.Printf("Retrieved %d rows from spreadsheet", len(data.Data))
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

	clean := func(s string) string {
		s = strings.TrimSpace(s)
		if s == "" || s == "-" {
			return "0"
		}

		var replacer = strings.NewReplacer("$", "", "₽", "", "€", "", "£", "", "%", "")

		s = strings.Join(strings.Fields(s), "")
		s = replacer.Replace(s)
		s = strings.ReplaceAll(s, ",", ".")

		return s
	}

	switch t := v.(type) {
	case float64:
		return t
	case int, int64:
		return float64(reflect.ValueOf(t).Int())
	case string:
		s := clean(t)
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			log.Printf("Error parsing float from '%s': %v", s, err)
			return 0
		}
		return f
	default:
		s := clean(fmt.Sprintf("%v", v))
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			log.Printf("Error parsing float from '%s': %v", s, err)
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
	if val == nil {
		return 0
	}

	var s = fmt.Sprintf("%v", val)

	s = strings.Map(
		func(r rune) rune {
			if unicode.IsSpace(r) {
				return ' '
			}
			return r
		},
		s,
	)

	s = strings.TrimSpace(s)
	if s == "" || s == "-" {
		return 0
	}

	s = strings.ReplaceAll(s, "$", "")
	s = strings.ReplaceAll(s, "₽", "")
	s = strings.ReplaceAll(s, "€", "")
	s = strings.ReplaceAll(s, "£", "")
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, ",", ".")
	s = strings.ReplaceAll(s, "%", "")

	var result = cast.ToFloat64(s)

	if result == 0 && s != "0" && s != "0.0" {
		log.Printf("Warning: could not parse '%v' as float, returning 0", val)
	}
	return result
}
