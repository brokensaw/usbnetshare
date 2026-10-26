package main

import (
	"os"
	"regexp"
	"testing"

	"github.com/dop251/goja/parser"
)

// La interfaz es un único HTML con el JavaScript dentro: si tiene un error de
// sintaxis no funciona nada (ni las pestañas). Este test lo detecta.
func TestFrontendScriptParses(t *testing.T) {
	b, err := os.ReadFile("frontend/index.html")
	if err != nil {
		t.Fatal(err)
	}
	scripts := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(string(b), -1)
	if len(scripts) == 0 {
		t.Fatal("no se encontró ningún <script>")
	}
	for i, m := range scripts {
		if _, err := parser.ParseFile(nil, "index.html", m[1], 0); err != nil {
			t.Errorf("script %d: %v", i, err)
		}
	}
}
