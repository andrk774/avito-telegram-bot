package main

import "testing"

const sample = `<html><body>
<div class="x" data-marker="item" data-item-id="4455667788">
  <a href="/sankt-peterburg/telefony/iphone_13_128gb_4455667788" data-marker="item-title"><span>iPhone 13 128GB синий</span></a>
  <meta itemprop="price" content="38000">
  <div data-marker="item-price">38&nbsp;000&nbsp;₽</div>
  <div data-marker="item-address"><span>Санкт-Петербург, Невский р-н</span></div>
</div>
<div class="x" data-marker="item" data-item-id="9988776655">
  <a href="/sankt-peterburg/telefony/iphone_13_replika_9988776655" data-marker="item-title">iPhone 13 реплика копия</a>
  <div data-marker="item-price">9&nbsp;900&nbsp;₽</div>
</div>
<div class="x" data-marker="item" data-item-id="1112223334">
  <a href="/sankt-peterburg/telefony/iphone_13_pro_1112223334" data-marker="item-title">iPhone 13 Pro 256</a>
  <meta itemprop="price" content="55000">
</div>
</body></html>`

func TestParseItems(t *testing.T) {
	items := parseItems(sample)
	if len(items) != 3 {
		t.Fatalf("ожидалось 3 карточки, получено %d", len(items))
	}
	if items[0].ID != "4455667788" || items[0].Price != 38000 {
		t.Errorf("карточка 0: %+v", items[0])
	}
	if items[0].Title != "iPhone 13 128GB синий" {
		t.Errorf("заголовок: %q", items[0].Title)
	}
	if items[0].URL != "https://www.avito.ru/sankt-peterburg/telefony/iphone_13_128gb_4455667788" {
		t.Errorf("url: %q", items[0].URL)
	}
	if items[0].Geo != "Санкт-Петербург, Невский р-н" {
		t.Errorf("гео: %q", items[0].Geo)
	}
}

func TestFilters(t *testing.T) {
	topic := Topic{MaxPrice: 40000, MinPrice: 10000, Exclude: []string{"реплика", "копия"}}
	items := parseItems(sample)
	got := []bool{}
	for _, it := range items {
		got = append(got, passesFilters(it, topic))
	}
	want := []bool{true, false, false} // норм, реплика(искл), 55к(>40к)
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("фильтр %d: got %v want %v (%s, %d)", i, got[i], want[i], items[i].Title, items[i].Price)
		}
	}
}

func TestSortByDate(t *testing.T) {
	got := sortByDate("https://www.avito.ru/rossiya/telefony?q=iphone+13")
	if got != "https://www.avito.ru/rossiya/telefony?q=iphone+13&s=104" {
		t.Errorf("sortByDate: %q", got)
	}
}

func TestFmtPrice(t *testing.T) {
	if fmtPrice(38000) != "38 000 ₽" {
		t.Errorf("fmtPrice: %q", fmtPrice(38000))
	}
	if fmtPrice(-1) != "цена не указана" {
		t.Errorf("fmtPrice(-1): %q", fmtPrice(-1))
	}
}

func TestStoreSeeAndDrop(t *testing.T) {
	dir := t.TempDir()
	s := openStore(dir + "/state.json")
	s.addTopic(Topic{ID: "t1", Name: "Тема"})

	// первое появление — новое
	wasNew, old := s.see("t1", "100", 500)
	if !wasNew || old != -1 {
		t.Fatalf("первое see: wasNew=%v old=%d", wasNew, old)
	}
	// та же цена — не новое, старая цена 500
	wasNew, old = s.see("t1", "100", 500)
	if wasNew || old != 500 {
		t.Fatalf("повтор see: wasNew=%v old=%d", wasNew, old)
	}
	// цена упала
	wasNew, old = s.see("t1", "100", 400)
	if wasNew || old != 500 {
		t.Fatalf("падение see: wasNew=%v old=%d", wasNew, old)
	}
	if s.priceCount("t1") != 1 {
		t.Fatalf("priceCount=%d", s.priceCount("t1"))
	}
	// удаление темы чистит цены
	if name, ok := s.delTopic("t1"); !ok || name != "Тема" {
		t.Fatalf("delTopic: %q %v", name, ok)
	}
	if s.priceCount("t1") != 0 {
		t.Fatalf("после удаления priceCount=%d", s.priceCount("t1"))
	}
}

func TestStorePersists(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/state.json"
	s := openStore(p)
	s.addTopic(Topic{ID: "x", Name: "N", MaxPrice: 1000})
	s.see("x", "1", 999)
	s.flush() // scanTopic сбрасывает цены на диск после прохода
	// перечитываем с диска
	s2 := openStore(p)
	if len(s2.topics()) != 1 || s2.topics()[0].Name != "N" {
		t.Fatalf("темы не сохранились: %+v", s2.topics())
	}
	if s2.priceCount("x") != 1 {
		t.Fatalf("цены не сохранились: %d", s2.priceCount("x"))
	}
}
