package account

import "testing"

func TestCollectImportItemsFromCPAWrapper(t *testing.T) {
	value := map[string]any{
		"items": []any{
			map[string]any{"accessToken": "a", "refresh_token": "r"},
			map[string]any{"access_token": "b", "type": "Plus"},
		},
	}
	items := collectImportItems(value)
	if len(items) != 2 || items[0].AccessToken != "a" || items[1].AccessToken != "b" {
		t.Fatalf("unexpected items: %#v", items)
	}
}

func TestParseImportJSONNestedWrapper(t *testing.T) {
	data := []byte(`{
		"data": {
			"items": [
				{"access_token": "token-a", "email": "a@example.com"},
				{"accessToken": "token-b", "type": "Plus"}
			]
		}
	}`)
	items, err := ParseImportJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].AccessToken != "token-a" || items[1].AccessToken != "token-b" {
		t.Fatalf("unexpected items: %#v", items)
	}
}

func TestParseImportJSONTokenList(t *testing.T) {
	items, err := ParseImportJSON([]byte(`{"tokens":["token-a","token-b"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].AccessToken != "token-a" || items[1].AccessToken != "token-b" {
		t.Fatalf("unexpected items: %#v", items)
	}
}
