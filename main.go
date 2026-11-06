// Avito -> Telegram watcher.
//
// Следит за заданными поисками на Avito и шлёт новые объявления в Telegram.
// Без внешних зависимостей — только стандартная библиотека Go.
//
// Темы описываются в topics.json. Токен бота — в переменной окружения BOT_TOKEN.
// chat_id бот запоминает сам, когда вы напишете ему /start.
//
// Сборка:  go build -o avito-bot .
// Запуск:  BOT_TOKEN=xxxx ./avito-bot
//
// Команды в чате: /start /help /list /check /status
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// Конфиг
// ---------------------------------------------------------------------------

type Topic struct {
	Name     string   `json:"name"`
	URL      string   `json:"url"`
	MaxPrice int      `json:"max_price"`
	MinPrice int      `json:"min_price"`
	Include  []string `json:"include"`
	Exclude  []string `json:"exclude"`
}

type Config struct {
	IntervalSeconds int     `json:"interval_seconds"`
	Topics          []Topic `json:"topics"`
}

const (
	userAgent    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36"
	avitoBase    = "https://www.avito.ru"
	reqTimeout   = 30 * time.Second
	blockWarnTTL = time.Hour
	minInterval  = 60
)

var (
	dir        = mustDir()
	topicsFile = filepath.Join(dir, "topics.json")
	stateFile  = filepath.Join(dir, "state.json")
	botToken   = strings.TrimSpace(os.Getenv("BOT_TOKEN"))
	avitoProxy = strings.TrimSpace(os.Getenv("AVITO_PROXY"))
)

func mustDir() string {
	exe, err := os.Executable()
	if err == nil {
		return filepath.Dir(exe)
	}
	wd, _ := os.Getwd()
	return wd
}

func loadConfig() (*Config, error) {
	data, err := os.ReadFile(topicsFile)
	if err != nil {
		return nil, fmt.Errorf("нет файла %s: %w", topicsFile, err)
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("topics.json: %w", err)
	}
	for i, t := range c.Topics {
		if t.Name == "" || t.URL == "" {
			return nil, fmt.Errorf("тема #%d: нужны name и url", i+1)
		}
	}
	if c.IntervalSeconds < minInterval {
		c.IntervalSeconds = 300
	}
	return &c, nil
}

// ---------------------------------------------------------------------------
// Хранилище (JSON-файл, потокобезопасно). Без внешней БД.
// ---------------------------------------------------------------------------

type Store struct {
	mu       sync.Mutex
	Seen     map[string]map[string]bool `json:"seen"`     // topic -> set id
	Settings map[string]string          `json:"settings"` // chat_id, tg_offset
	path     string
}

func openStore(path string) *Store {
	s := &Store{
		Seen:     map[string]map[string]bool{},
		Settings: map[string]string{},
		path:     path,
	}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, s)
		if s.Seen == nil {
			s.Seen = map[string]map[string]bool{}
		}
		if s.Settings == nil {
			s.Settings = map[string]string{}
		}
	}
	return s
}

// save вызывать под залоченным s.mu.
func (s *Store) save() {
	tmp := s.path + ".tmp"
	data, _ := json.MarshalIndent(s, "", "  ")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Printf("state: %v", err)
		return
	}
	_ = os.Rename(tmp, s.path)
}

func (s *Store) getSetting(k string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Settings[k]
}

func (s *Store) setSetting(k, v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Settings[k] = v
	s.save()
}

func (s *Store) seenCount(topic string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Seen[topic])
}

// markSeen помечает id как виденный, возвращает true, если он был новым.
func (s *Store) markSeen(topic, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Seen[topic] == nil {
		s.Seen[topic] = map[string]bool{}
	}
	if s.Seen[topic][id] {
		return false
	}
	s.Seen[topic][id] = true
	return true
}

func (s *Store) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.save()
}

// ---------------------------------------------------------------------------
// Загрузка и разбор страницы Avito (regexp по стабильным data-marker).
// ---------------------------------------------------------------------------

type Item struct {
	ID    string
	Title string
	URL   string
	Price int // -1 = не указана
	Geo   string
}

var httpClient = func() *http.Client {
	tr := &http.Transport{}
	if avitoProxy != "" {
		if pu, err := url.Parse(avitoProxy); err == nil {
			tr.Proxy = http.ProxyURL(pu)
		}
	}
	return &http.Client{Timeout: reqTimeout, Transport: tr}
}()

var (
	reItemMarker = regexp.MustCompile(`data-marker="item"`)
	reItemID     = regexp.MustCompile(`data-item-id="(\d+)"`)
	reTitleTag   = regexp.MustCompile(`(?s)<a[^>]*data-marker="item-title"[^>]*>(.*?)</a>`)
	reHref       = regexp.MustCompile(`href="([^"]+)"`)
	rePriceMeta  = regexp.MustCompile(`itemprop="price"[^>]*content="(\d+)"`)
	rePriceText  = regexp.MustCompile(`(?s)data-marker="item-price"[^>]*>(.*?)</`)
	reAddr       = regexp.MustCompile(`(?s)data-marker="item-address"[^>]*>(.*?)</div>`)
	reTag        = regexp.MustCompile(`<[^>]+>`)
	reDigits     = regexp.MustCompile(`\d+`)
	reIDfromURL  = regexp.MustCompile(`_(\d+)(?:\?|$|/)`)
)

func stripTags(s string) string {
	return strings.TrimSpace(html.UnescapeString(reTag.ReplaceAllString(s, "")))
}

// parseItems разбирает карточки выдачи. Avito помечает элементы атрибутами
// data-marker — они стабильнее классов. Если Avito сменит разметку, править
// нужно ТОЛЬКО регулярки выше и эту функцию.
func parseItems(body string) []Item {
	locs := reItemMarker.FindAllStringIndex(body, -1)
	var items []Item
	for i, loc := range locs {
		next := len(body)
		if i+1 < len(locs) {
			next = locs[i+1][0]
		}
		card := body[loc[0]:next]

		// id: ищем в узком окне вокруг маркера (тот же тег), затем в карточке.
		idWin := body[maxi(0, loc[0]-200):mini(len(body), loc[0]+200)]
		id := first(reItemID, idWin)
		if id == "" {
			id = first(reItemID, card)
		}

		tm := reTitleTag.FindStringSubmatch(card)
		if tm == nil {
			continue
		}
		title := stripTags(tm[1])
		href := first(reHref, tm[0])
		link := href
		if strings.HasPrefix(href, "/") {
			link = avitoBase + href
		}
		if id == "" {
			if m := reIDfromURL.FindStringSubmatch(link); m != nil {
				id = m[1]
			} else {
				id = link
			}
		}

		price := -1
		if p := first(rePriceMeta, card); p != "" {
			price, _ = strconv.Atoi(p)
		} else if pt := first(rePriceText, card); pt != "" {
			if d := strings.Join(reDigits.FindAllString(stripTags(pt), -1), ""); d != "" {
				price, _ = strconv.Atoi(d)
			}
		}

		geo := ""
		if g := first(reAddr, card); g != "" {
			geo = truncate(stripTags(g), 80)
		}

		items = append(items, Item{ID: id, Title: title, URL: link, Price: price, Geo: geo})
	}
	return items
}

func passesFilters(it Item, t Topic) bool {
	low := strings.ToLower(it.Title)
	for _, w := range t.Exclude {
		if strings.Contains(low, strings.ToLower(w)) {
			return false
		}
	}
	for _, w := range t.Include {
		if !strings.Contains(low, strings.ToLower(w)) {
			return false
		}
	}
	if it.Price >= 0 {
		if t.MinPrice > 0 && it.Price < t.MinPrice {
			return false
		}
		if t.MaxPrice > 0 && it.Price > t.MaxPrice {
			return false
		}
	}
	return true
}

// sortByDate добавляет s=104 ("сначала новые"), если параметра нет.
func sortByDate(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	if q.Get("s") == "" {
		q.Set("s", "104")
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func looksBlocked(status int, body string) bool {
	if status == http.StatusForbidden || status == http.StatusTooManyRequests {
		return true
	}
	low := strings.ToLower(body)
	bad := []string{"доступ ограничен", "подтвердите, что запросы", "captcha", "firewall", "необычн"}
	if len(body) < 3000 {
		for _, b := range bad {
			if strings.Contains(low, b) {
				return true
			}
		}
	}
	return false
}

// fetch возвращает (html, blocked).
func fetch(ctx context.Context, raw string) (string, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "ru-RU,ru;q=0.9,en;q=0.8")
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("сеть: %v", err)
		return "", false
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 6<<20))
	body := string(data)
	if looksBlocked(resp.StatusCode, body) {
		return "", true
	}
	if resp.StatusCode != http.StatusOK {
		log.Printf("HTTP %d для %s", resp.StatusCode, raw)
		return "", false
	}
	return body, false
}

// ---------------------------------------------------------------------------
// Telegram
// ---------------------------------------------------------------------------

type tgResp struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
}

func tgCall(method string, params map[string]any) (json.RawMessage, bool) {
	buf, _ := json.Marshal(params)
	u := fmt.Sprintf("https://api.telegram.org/bot%s/%s", botToken, method)
	resp, err := http.Post(u, "application/json", strings.NewReader(string(buf)))
	if err != nil {
		log.Printf("telegram %s: %v", method, err)
		return nil, false
	}
	defer resp.Body.Close()
	var r tgResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, false
	}
	return r.Result, r.OK
}

func notify(st *Store, text string, preview bool) {
	chatID := st.getSetting("chat_id")
	if chatID == "" {
		log.Printf("нет chat_id — напишите боту /start")
		return
	}
	tgCall("sendMessage", map[string]any{
		"chat_id":                  chatID,
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": !preview,
	})
}

func fmtPrice(p int) string {
	if p < 0 {
		return "цена не указана"
	}
	s := strconv.Itoa(p)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ' ')
		}
		out = append(out, c)
	}
	return string(out) + " ₽"
}

func sendItem(st *Store, topic string, it Item) {
	var b strings.Builder
	fmt.Fprintf(&b, "\U0001f195 <b>%s</b>\n", html.EscapeString(topic))
	fmt.Fprintf(&b, "%s\n", html.EscapeString(it.Title))
	fmt.Fprintf(&b, "\U0001f4b0 %s", fmtPrice(it.Price))
	if it.Geo != "" {
		fmt.Fprintf(&b, "\n\U0001f4cd %s", html.EscapeString(it.Geo))
	}
	fmt.Fprintf(&b, "\n%s", it.URL)
	notify(st, b.String(), true)
}

// ---------------------------------------------------------------------------
// Слежение
// ---------------------------------------------------------------------------

var blockWarned sync.Map // topic -> time.Time

func scanTopic(ctx context.Context, st *Store, t Topic) {
	body, blocked := fetch(ctx, sortByDate(t.URL))
	if blocked {
		if v, ok := blockWarned.Load(t.Name); !ok || time.Since(v.(time.Time)) > blockWarnTTL {
			blockWarned.Store(t.Name, time.Now())
			notify(st, fmt.Sprintf("⚠️ Avito заблокировал запрос по теме «%s» (антибот/капча). "+
				"Если бот на VPS — нужен жилой прокси (AVITO_PROXY) или запуск с домашнего IP.",
				html.EscapeString(t.Name)), false)
		}
		log.Printf("[%s] блокировка", t.Name)
		return
	}
	if body == "" {
		return
	}

	items := parseItems(body)
	if len(items) == 0 {
		log.Printf("[%s] карточек не найдено (возможно, сменилась вёрстка Avito)", t.Name)
		return
	}

	firstRun := st.seenCount(t.Name) == 0
	var fresh []Item
	for _, it := range items {
		isNew := st.markSeen(t.Name, it.ID)
		if !isNew || firstRun {
			continue
		}
		if passesFilters(it, t) {
			fresh = append(fresh, it)
		}
	}
	st.flush()

	if firstRun {
		notify(st, fmt.Sprintf("\U0001f440 Начал следить за темой «%s». Сейчас в выдаче %d объявлений — "+
			"буду присылать только новые.", html.EscapeString(t.Name), len(items)), false)
		log.Printf("[%s] первичная индексация: %d", t.Name, len(items))
		return
	}

	for i := len(fresh) - 1; i >= 0; i-- { // от старых к новым
		sendItem(st, t.Name, fresh[i])
		time.Sleep(time.Second)
	}
	if len(fresh) > 0 {
		log.Printf("[%s] новых: %d", t.Name, len(fresh))
	}
}

func watcherLoop(ctx context.Context, st *Store) {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("конфиг: %v", err)
	}
	log.Printf("слежу за %d темами, интервал %d сек", len(cfg.Topics), cfg.IntervalSeconds)
	for {
		cfg, err = loadConfig() // перечитываем, чтобы правки topics.json подхватывались
		if err != nil {
			log.Printf("конфиг: %v", err)
		} else {
			for _, t := range cfg.Topics {
				select {
				case <-ctx.Done():
					return
				default:
				}
				scanTopic(ctx, st, t)
				sleepCtx(ctx, time.Duration(3+rand.Intn(5))*time.Second)
			}
		}
		base := time.Duration(cfg.IntervalSeconds) * time.Second
		if !sleepCtx(ctx, base+time.Duration(rand.Int63n(int64(base/5+1)))) {
			return
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ---------------------------------------------------------------------------
// Команды Telegram (long polling)
// ---------------------------------------------------------------------------

const helpText = "Я слежу за Avito по вашим темам и присылаю новые объявления.\n\n" +
	"/list — показать темы\n/check — проверить сейчас\n/status — состояние\n/help — помощь\n\n" +
	"Темы редактируются в topics.json."

type tgUpdate struct {
	UpdateID int `json:"update_id"`
	Message  *struct {
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		Text string `json:"text"`
	} `json:"message"`
}

func handleCommand(ctx context.Context, st *Store, text string, chatID int64) {
	cmd := strings.ToLower(strings.TrimPrefix(strings.Fields(text)[0], "/"))
	if i := strings.IndexByte(cmd, '@'); i >= 0 {
		cmd = cmd[:i]
	}
	switch cmd {
	case "start":
		st.setSetting("chat_id", strconv.FormatInt(chatID, 10))
		notify(st, "✅ Готово, буду писать сюда.\n\n"+helpText, false)
	case "help":
		notify(st, helpText, false)
	case "list":
		cfg, err := loadConfig()
		if err != nil {
			notify(st, "Ошибка конфига: "+html.EscapeString(err.Error()), false)
			return
		}
		var b strings.Builder
		b.WriteString("\U0001f4cb <b>Темы:</b>")
		for _, t := range cfg.Topics {
			var ex []string
			if t.MaxPrice > 0 {
				ex = append(ex, "до "+fmtPrice(t.MaxPrice))
			}
			if len(t.Include) > 0 {
				ex = append(ex, "вкл: "+strings.Join(t.Include, ", "))
			}
			if len(t.Exclude) > 0 {
				ex = append(ex, "искл: "+strings.Join(t.Exclude, ", "))
			}
			tail := ""
			if len(ex) > 0 {
				tail = " — " + strings.Join(ex, "; ")
			}
			fmt.Fprintf(&b, "\n• %s", html.EscapeString(t.Name+tail))
		}
		notify(st, b.String(), false)
	case "check":
		notify(st, "\U0001f504 Проверяю сейчас…", false)
		cfg, err := loadConfig()
		if err == nil {
			for _, t := range cfg.Topics {
				scanTopic(ctx, st, t)
			}
		}
		notify(st, "✅ Проверка завершена.", false)
	case "status":
		cfg, err := loadConfig()
		total := 0
		n := 0
		if err == nil {
			n = len(cfg.Topics)
			for _, t := range cfg.Topics {
				total += st.seenCount(t.Name)
			}
		}
		px := "нет"
		if avitoProxy != "" {
			px = "да"
		}
		notify(st, fmt.Sprintf("\U0001f916 Работаю.\nТем: %d\nЗапомнено объявлений: %d\nПрокси: %s", n, total, px), false)
	default:
		notify(st, "Не понял команду. /help", false)
	}
}

func telegramLoop(ctx context.Context, st *Store) {
	offset := 0
	if v := st.getSetting("tg_offset"); v != "" {
		offset, _ = strconv.Atoi(v)
	}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		res, ok := tgCall("getUpdates", map[string]any{"offset": offset, "timeout": 25})
		if !ok {
			sleepCtx(ctx, 3*time.Second)
			continue
		}
		var updates []tgUpdate
		if err := json.Unmarshal(res, &updates); err != nil {
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			st.setSetting("tg_offset", strconv.Itoa(offset))
			if u.Message == nil {
				continue
			}
			text := strings.TrimSpace(u.Message.Text)
			if strings.HasPrefix(text, "/") {
				handleCommand(ctx, st, text, u.Message.Chat.ID)
			}
		}
	}
}

// ---------------------------------------------------------------------------

func maxi(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func mini(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func first(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func main() {
	log.SetFlags(log.Ltime)
	if botToken == "" {
		log.Fatal("задайте переменную окружения BOT_TOKEN (токен от @BotFather)")
	}
	if _, err := loadConfig(); err != nil {
		log.Fatal(err)
	}
	st := openStore(stateFile)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go watcherLoop(ctx, st)
	log.Printf("бот запущен; напишите ему /start в Telegram")
	telegramLoop(ctx, st)
	log.Printf("остановлено")
}
