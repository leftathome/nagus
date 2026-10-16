package promo

import "testing"

func read(t *testing.T, s string) Reading {
	t.Helper()
	r, err := ParseReading(s)
	if err != nil {
		t.Fatalf("ParseReading(%s): %v", s, err)
	}
	return r
}

func TestAgree(t *testing.T) {
	drive := product("WD Red Plus 12TB NAS Internal Hard Drive", "219.99", "279.99")
	for _, tc := range []struct {
		name     string
		a, b     string
		drop     string
		price    string
		was      string
		brand    string
		titleSub string
	}{
		{name: "agree", a: drive, b: drive, price: "219.99", was: "279.99"},
		{name: "dollar sign and comma", a: product("Big Drive 24TB Hard Drive", "$1,299.99", ""), b: product("Big Drive 24TB Hard Drive", "1299.99", ""), price: "1299.99"},
		{name: "same cents different text", a: product("Drive 12TB Hard Drive", "219.9", ""), b: product("Drive 12TB Hard Drive", "219.90", ""), price: "219.9"},
		{name: "number price", a: `{"products":[{"title":"WD Red Plus 12TB NAS Internal Hard Drive","price":219.99,"was_price":null,"brand":null,"currency":null}]}`, b: drive, price: "219.99"},
		{name: "price disagree", a: drive, b: product("WD Red Plus 12TB NAS Internal Hard Drive", "229.99", ""), drop: DropPriceDisagree},
		{name: "title disagree", a: drive, b: product("Seagate IronWolf Pro 18TB", "219.99", ""), drop: DropTitleDisagree},
		{name: "title roughly agrees", a: drive, b: product("WD Red Plus 12TB NAS Hard Drive", "219.99", ""), price: "219.99"},
		{name: "was disagree kept product", a: drive, b: product("WD Red Plus 12TB NAS Internal Hard Drive", "219.99", "299.99"), price: "219.99"},
		{name: "was below price", a: product("Drive 12TB Hard Drive", "219.99", "199.99"), b: product("Drive 12TB Hard Drive", "219.99", "199.99"), drop: DropPriceSanity},
		{name: "was equal price", a: product("Drive 12TB Hard Drive", "219.99", "219.99"), b: product("Drive 12TB Hard Drive", "219.99", "219.99"), drop: DropPriceSanity},
		{name: "no product", a: `{"products":[]}`, b: drive, drop: DropNoProduct},
		{name: "multi", a: `{"products":[{"title":"A 1TB Hard Drive","price":"1.00","was_price":null,"brand":null,"currency":null},{"title":"B","price":"2.00","was_price":null,"brand":null,"currency":null}]}`, b: drive, drop: DropMultiProduct},
		{name: "multi vs none", a: `{"products":[{"title":"A","price":"1.00","was_price":null,"brand":null,"currency":null},{"title":"B","price":"2.00","was_price":null,"brand":null,"currency":null}]}`, b: `{"products":[]}`, drop: DropMultiProduct},
		{name: "null price", a: `{"products":[{"title":"Drive","price":null,"was_price":null,"brand":null,"currency":null}]}`, b: drive, drop: DropBadPrice},
		{name: "zero price", a: product("Drive", "0.00", ""), b: product("Drive", "0.00", ""), drop: DropBadPrice},
		{name: "negative price", a: product("Drive", "-5.00", ""), b: product("Drive", "-5.00", ""), drop: DropBadPrice},
		{name: "three decimals", a: product("Drive", "5.999", ""), b: product("Drive", "5.999", ""), drop: DropBadPrice},
		{name: "text price", a: product("Drive", "call for price", ""), b: product("Drive", "call for price", ""), drop: DropBadPrice},
		{name: "euro", a: `{"products":[{"title":"Drive","price":"9.99","was_price":null,"brand":null,"currency":"EUR"}]}`, b: drive, drop: DropCurrency},
		{name: "empty title", a: product("  ", "9.99", ""), b: product("Drive", "9.99", ""), drop: DropNoTitle},
		{
			name: "brand agreed", a: `{"products":[{"title":"Tolaini Al Passo Toscana 2022","price":"27.97","was_price":"29","brand":"Tolaini","currency":"USD"}]}`,
			b: `{"products":[{"title":"Tolaini Al Passo Toscana 2022","price":"27.97","was_price":"29.00","brand":"tolaini","currency":null}]}`, price: "27.97", was: "29", brand: "Tolaini",
		},
		{
			name: "brand not agreed", a: `{"products":[{"title":"Tolaini Al Passo Toscana 2022","price":"27.97","was_price":null,"brand":"Tolaini","currency":"USD"}]}`,
			b: `{"products":[{"title":"Tolaini Al Passo Toscana 2022","price":"27.97","was_price":null,"brand":"Al Passo","currency":"USD"}]}`, price: "27.97",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, drop := Agree(read(t, tc.a), read(t, tc.b))
			if drop != tc.drop {
				t.Fatalf("drop = %q, want %q (product %+v)", drop, tc.drop, p)
			}
			if drop != "" {
				return
			}
			if p.Price != tc.price || p.WasPrice != tc.was || p.Brand != tc.brand {
				t.Fatalf("product %+v, want price %q was %q brand %q", p, tc.price, tc.was, tc.brand)
			}
		})
	}
}

func TestCategoryFit(t *testing.T) {
	for _, tc := range []struct {
		cat, title string
		want       bool
	}{
		{"hdd", "WD Red Plus 12TB NAS Internal Hard Drive", true},
		{"hdd", "Seagate IronWolf Pro 18TB", true},
		{"hdd", "Seagate Exos X20 20TB SATA 7200 RPM", true},
		{"hdd", "MSI Summit E13 Flip Evo 13.4in Laptop 1TB SSD", false},
		{"hdd", "ASUS Gaming Desktop PC 2TB HDD", false},
		{"hdd", "WD Elements 12TB external hard drive enclosure", false},
		{"hdd", "Hard Drive", false},
		{"hdd", "AMD Radeon RX 9070 XT 16GB Graphics Card", false},
		{"wine", "Tolaini Al Passo Toscana 2022", true},
	} {
		if got := categoryFit(tc.cat, tc.title); got != tc.want {
			t.Errorf("categoryFit(%s, %q) = %v", tc.cat, tc.title, got)
		}
	}
}
