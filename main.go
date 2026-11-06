// Avito -> Telegram watcher.
//
// Следит за заданными поисками на Avito и шлёт новые объявления в Telegram,
// а также сообщает о падении цены на уже виденных объявлениях.
// Без внешних зависимостей — только стандартная библиотека Go.
//
// Токен бота — в переменной окружения BOT_TOKEN. chat_id бот запоминает сам,
// когда вы напишете ему /start. Темы можно задать в topics.json (начальный
// список) и дальше управлять ими прямо из чата: /add, /del, кнопки.
//
// Сборка:  go build -o avito-bot .
// Запуск:  BOT_TOKEN=xxxx ./avito-bot
package main

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"math/rand/v2"
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

// version подставляется при релизной сборке (-X main.version=...).
var version = "dev"

// ---------------------------------------------------------------------------
// Типы
// ---------------------------------------------------------------------------

type Topic struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	URL      string   `json:"url"`
	MaxPrice int      `json:"max_price"`
	MinPrice int      `json:"min_price"`
	Include  []string `json:"include"`
	Exclude  []string `json:"exclude"`
}

// seedConfig — формат topics.json (начальный список тем).
type seedConfig struct {
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
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	wd, _ := os.Getwd()
	return wd
}

func genID() string {
	b := make([]byte, 4)
	_, _ = crand.Read(b)
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// Хранилище (JSON-файл, потокобезопасно). Темы живут здесь, topics.json —
// только начальный сид при первом запуске.
// ---------------------------------------------------------------------------

type Store struct {
	mu       sync.Mutex
	Prices   map[string]map[string]int `json:"prices"`   // topicID -> itemID -> последняя цена (-1 если неизвестна)
	Topics   []Topic                   `json:"topics"`   // актуальный список тем
	Interval int                       `json:"interval"` // период опроса, сек
	Settings map[string]string         `json:"settings"` // chat_id, tg_offset
	path     string
}

func openStore(path string) *Store {
	s := &Store{
		Prices:   map[string]map[string]int{},
		Settings: map[string]string{},
		Interval: 300,
		path:     path,
	}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, s)
		if s.Prices == nil {
			s.Prices = map[string]map[string]int{}
		}
		if s.Settings == nil {
			s.Settings = map[string]string{}
		}
	}
	return s
}

func (s *Store) save() { // вызывать под s.mu
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

func (s *Store) topics() []Topic {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Topic(nil), s.Topics...)
}

func (s *Store) interval() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Interval < minInterval {
		return 300
	}
	return s.Interval
}

func (s *Store) addTopic(t Topic) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Topics = append(s.Topics, t)
	s.save()
}

// delTopic удаляет тему по ID, возвращает её имя и успех.
func (s *Store) delTopic(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.Topics {
		if t.ID == id {
			s.Topics = append(s.Topics[:i], s.Topics[i+1:]...)
			delete(s.Prices, id)
			s.save()
			return t.Name, true
		}
	}
	return "", false
}

func (s *Store) seedIfEmpty() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Topics) > 0 {
		return
	}
	data, err := os.ReadFile(topicsFile)
	if err != nil {
		return
	}
	var sc seedConfig
	if json.Unmarshal(data, &sc) != nil {
		return
	}
	if sc.IntervalSeconds >= minInterval {
		s.Interval = sc.IntervalSeconds
	}
	for _, t := range sc.Topics {
		if t.ID == "" {
			t.ID = genID()
		}
		s.Topics = append(s.Topics, t)
	}
	s.save()
	log.Printf("сид из topics.json: %d тем", len(sc.Topics))
}

func (s *Store) priceCount(topicID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Prices[topicID])
}

// see записывает текущую цену объявления и возвращает:
// wasNew — объявления раньше не было; oldPrice — прошлая цена (или -1).
func (s *Store) see(topicID, itemID string, price int) (wasNew bool, oldPrice int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.Prices[topicID]
	if m == nil {
		m = map[string]int{}
		s.Prices[topicID] = m
	}
	old, ok := m[itemID]
	m[itemID] = price
	if !ok {
		return true, -1
	}
	return false, old
}

func (s *Store) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.save()
}

// ---------------------------------------------------------------------------
// Загрузка и разбор страницы Avito
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
	if json.NewDecoder(resp.Body).Decode(&r) != nil {
		return nil, false
	}
	return r.Result, r.OK
}

func send(chatID, text string, markup any) {
	p := map[string]any{
		"chat_id":                  chatID,
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}
	if markup != nil {
		p["reply_markup"] = markup
	}
	tgCall("sendMessage", p)
}

// notify шлёт владельцу (в сохранённый chat_id).
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

func inlineKeyboard(rows ...[]map[string]any) map[string]any {
	return map[string]any{"inline_keyboard": rows}
}

func btn(text, data string) map[string]any {
	return map[string]any{"text": text, "callback_data": data}
}

func mainMenu() map[string]any {
	return inlineKeyboard(
		[]map[string]any{btn("📋 Темы", "list"), btn("🔍 Проверить", "check")},
		[]map[string]any{btn("➕ Добавить", "add"), btn("ℹ️ Статус", "status")},
	)
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

func sendPriceDrop(st *Store, topic string, it Item, old int) {
	var b strings.Builder
	fmt.Fprintf(&b, "\U0001f4c9 <b>Цена упала — %s</b>\n", html.EscapeString(topic))
	fmt.Fprintf(&b, "%s\n", html.EscapeString(it.Title))
	fmt.Fprintf(&b, "было %s → стало <b>%s</b>", fmtPrice(old), fmtPrice(it.Price))
	if it.Price >= 0 && old > 0 {
		fmt.Fprintf(&b, " (−%s)", fmtPrice(old-it.Price))
	}
	fmt.Fprintf(&b, "\n%s", it.URL)
	notify(st, b.String(), true)
}

// ---------------------------------------------------------------------------
// Слежение
// ---------------------------------------------------------------------------

var blockWarned sync.Map // topicID -> time.Time

func scanTopic(ctx context.Context, st *Store, t Topic) {
	body, blocked := fetch(ctx, sortByDate(t.URL))
	if blocked {
		if v, ok := blockWarned.Load(t.ID); !ok || time.Since(v.(time.Time)) > blockWarnTTL {
			blockWarned.Store(t.ID, time.Now())
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

	firstRun := st.priceCount(t.ID) == 0
	type drop struct {
		it  Item
		old int
	}
	var fresh []Item
	var drops []drop
	for _, it := range items {
		wasNew, old := st.see(t.ID, it.ID, it.Price)
		if firstRun {
			continue
		}
		if wasNew {
			if passesFilters(it, t) {
				fresh = append(fresh, it)
			}
		} else if it.Price >= 0 && old >= 0 && it.Price < old && passesFilters(it, t) {
			drops = append(drops, drop{it, old})
		}
	}
	st.flush()

	if firstRun {
		notify(st, fmt.Sprintf("\U0001f440 Начал следить за темой «%s». Сейчас в выдаче %d объявлений — "+
			"буду присылать новые и сообщать о падении цены.", html.EscapeString(t.Name), len(items)), false)
		log.Printf("[%s] первичная индексация: %d", t.Name, len(items))
		return
	}

	for i := len(fresh) - 1; i >= 0; i-- { // от старых к новым
		sendItem(st, t.Name, fresh[i])
		time.Sleep(time.Second)
	}
	for _, d := range drops {
		sendPriceDrop(st, t.Name, d.it, d.old)
		time.Sleep(time.Second)
	}
	if len(fresh) > 0 || len(drops) > 0 {
		log.Printf("[%s] новых: %d, подешевело: %d", t.Name, len(fresh), len(drops))
	}
}

func watcherLoop(ctx context.Context, st *Store) {
	log.Printf("слежу за %d темами, интервал %d сек", len(st.topics()), st.interval())
	for {
		for _, t := range st.topics() {
			select {
			case <-ctx.Done():
				return
			default:
			}
			scanTopic(ctx, st, t)
			sleepCtx(ctx, time.Duration(3+rand.IntN(5))*time.Second)
		}
		base := time.Duration(st.interval()) * time.Second
		if !sleepCtx(ctx, base+time.Duration(rand.Int64N(int64(base/5+1)))) {
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
// Диалог добавления темы (/add), состояние в памяти
// ---------------------------------------------------------------------------

type addFlow struct {
	step int // 0 ждём имя, 1 ждём url, 2 ждём макс.цену
	name string
	url  string
}

var (
	flows   = map[string]*addFlow{}
	flowsMu sync.Mutex
)

func startAdd(chatID string) {
	flowsMu.Lock()
	flows[chatID] = &addFlow{step: 0}
	flowsMu.Unlock()
	send(chatID, "➕ <b>Новая тема</b>\nПришлите <b>название</b> темы (как подписывать уведомления).\n\n/cancel — отмена", nil)
}

// handleFlow обрабатывает сообщение, если для чата идёт диалог. Возвращает true,
// если сообщение «съедено» диалогом.
func handleFlow(st *Store, chatID, text string) bool {
	flowsMu.Lock()
	f, ok := flows[chatID]
	flowsMu.Unlock()
	if !ok {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(text), "/cancel") {
		flowsMu.Lock()
		delete(flows, chatID)
		flowsMu.Unlock()
		send(chatID, "Отменено.", mainMenu())
		return true
	}
	text = strings.TrimSpace(text)
	switch f.step {
	case 0:
		if text == "" {
			send(chatID, "Пустое имя. Пришлите название темы.", nil)
			return true
		}
		f.name = text
		f.step = 1
		send(chatID, "Теперь пришлите <b>ссылку на поиск Avito</b> (откройте поиск с фильтрами и скопируйте URL).", nil)
	case 1:
		if !strings.Contains(text, "avito.") || !strings.HasPrefix(text, "http") {
			send(chatID, "Это не похоже на ссылку Avito. Пришлите URL вида https://www.avito.ru/...", nil)
			return true
		}
		f.url = text
		f.step = 2
		send(chatID, "Максимальная цена в рублях? Пришлите число или «-», чтобы без ограничения.", nil)
	case 2:
		maxp := 0
		if text != "-" && text != "" {
			digits := strings.Join(reDigits.FindAllString(text, -1), "")
			maxp, _ = strconv.Atoi(digits)
		}
		t := Topic{ID: genID(), Name: f.name, URL: f.url, MaxPrice: maxp}
		st.addTopic(t)
		flowsMu.Lock()
		delete(flows, chatID)
		flowsMu.Unlock()
		lim := "без ограничения цены"
		if maxp > 0 {
			lim = "до " + fmtPrice(maxp)
		}
		send(chatID, fmt.Sprintf("✅ Тема «%s» добавлена (%s). При первом проходе запомню текущие объявления и дальше буду слать новые.",
			html.EscapeString(f.name), lim), mainMenu())
	}
	return true
}

// ---------------------------------------------------------------------------
// Команды и кнопки
// ---------------------------------------------------------------------------

const helpText = "Я слежу за Avito по вашим темам: шлю новые объявления и сообщаю о падении цены.\n\n" +
	"<b>Команды</b>\n" +
	"/list — темы (с кнопкой удаления)\n" +
	"/add — добавить тему пошагово\n" +
	"/del — удалить тему\n" +
	"/check — проверить сейчас\n" +
	"/status — состояние\n" +
	"/menu — кнопки\n\n" +
	"Темы можно задать и в topics.json (начальный список)."

func listText(st *Store) (string, map[string]any) {
	topics := st.topics()
	if len(topics) == 0 {
		return "Пока нет ни одной темы. Добавьте через /add.", mainMenu()
	}
	var b strings.Builder
	b.WriteString("\U0001f4cb <b>Темы:</b>")
	var rows [][]map[string]any
	for _, t := range topics {
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
		rows = append(rows, []map[string]any{btn("🗑 "+truncate(t.Name, 24), "del:"+t.ID)})
	}
	return b.String(), inlineKeyboard(rows...)
}

func statusText(st *Store) string {
	topics := st.topics()
	total := 0
	for _, t := range topics {
		total += st.priceCount(t.ID)
	}
	px := "нет"
	if avitoProxy != "" {
		px = "да"
	}
	return fmt.Sprintf("\U0001f916 Работаю (%s).\nТем: %d\nИнтервал: %d сек\nОтслеживаю объявлений: %d\nПрокси: %s",
		version, len(topics), st.interval(), total, px)
}

func runCheck(ctx context.Context, st *Store, chatID string) {
	send(chatID, "\U0001f504 Проверяю сейчас…", nil)
	for _, t := range st.topics() {
		scanTopic(ctx, st, t)
	}
	send(chatID, "✅ Проверка завершена.", mainMenu())
}

func handleCommand(ctx context.Context, st *Store, text, chatID string) {
	cmd := strings.ToLower(strings.TrimPrefix(strings.Fields(text)[0], "/"))
	if i := strings.IndexByte(cmd, '@'); i >= 0 {
		cmd = cmd[:i]
	}
	switch cmd {
	case "start":
		st.setSetting("chat_id", chatID)
		send(chatID, "✅ Готово, буду писать сюда.\n\n"+helpText, mainMenu())
	case "help":
		send(chatID, helpText, mainMenu())
	case "menu":
		send(chatID, "Меню:", mainMenu())
	case "list", "del":
		txt, kb := listText(st)
		if cmd == "del" && len(st.topics()) > 0 {
			txt = "Нажмите на тему, чтобы удалить:\n\n" + txt
		}
		send(chatID, txt, kb)
	case "add":
		startAdd(chatID)
	case "check":
		runCheck(ctx, st, chatID)
	case "status":
		send(chatID, statusText(st), mainMenu())
	default:
		send(chatID, "Не понял команду. /help", mainMenu())
	}
}

func handleCallback(ctx context.Context, st *Store, data, chatID, cbID string) {
	// мгновенно гасим «часики» на кнопке
	tgCall("answerCallbackQuery", map[string]any{"callback_query_id": cbID})

	switch {
	case data == "list":
		txt, kb := listText(st)
		send(chatID, txt, kb)
	case data == "check":
		runCheck(ctx, st, chatID)
	case data == "add":
		startAdd(chatID)
	case data == "status":
		send(chatID, statusText(st), mainMenu())
	case strings.HasPrefix(data, "del:"):
		id := strings.TrimPrefix(data, "del:")
		if name, ok := st.delTopic(id); ok {
			send(chatID, "🗑 Удалено: «"+html.EscapeString(name)+"»", nil)
			txt, kb := listText(st)
			send(chatID, txt, kb)
		} else {
			send(chatID, "Тема уже удалена.", mainMenu())
		}
	}
}

// ---------------------------------------------------------------------------
// Long polling
// ---------------------------------------------------------------------------

type tgUpdate struct {
	UpdateID int `json:"update_id"`
	Message  *struct {
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		Text string `json:"text"`
	} `json:"message"`
	CallbackQuery *struct {
		ID   string `json:"id"`
		Data string `json:"data"`
		Msg  *struct {
			Chat struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
	} `json:"callback_query"`
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
		res, ok := tgCall("getUpdates", map[string]any{
			"offset":          offset,
			"timeout":         25,
			"allowed_updates": []string{"message", "callback_query"},
		})
		if !ok {
			sleepCtx(ctx, 3*time.Second)
			continue
		}
		var updates []tgUpdate
		if json.Unmarshal(res, &updates) != nil {
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			st.setSetting("tg_offset", strconv.Itoa(offset))

			if u.CallbackQuery != nil && u.CallbackQuery.Msg != nil {
				chatID := strconv.FormatInt(u.CallbackQuery.Msg.Chat.ID, 10)
				handleCallback(ctx, st, u.CallbackQuery.Data, chatID, u.CallbackQuery.ID)
				continue
			}
			if u.Message == nil {
				continue
			}
			chatID := strconv.FormatInt(u.Message.Chat.ID, 10)
			text := strings.TrimSpace(u.Message.Text)
			if strings.HasPrefix(text, "/") {
				// команда прерывает любой диалог добавления
				if !strings.EqualFold(text, "/cancel") {
					flowsMu.Lock()
					delete(flows, chatID)
					flowsMu.Unlock()
				}
				handleCommand(ctx, st, text, chatID)
				continue
			}
			if handleFlow(st, chatID, text) {
				continue
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
	st := openStore(stateFile)
	st.seedIfEmpty()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go watcherLoop(ctx, st)
	log.Printf("бот запущен (%s); напишите ему /start в Telegram", version)
	telegramLoop(ctx, st)
	log.Printf("остановлено")
}
