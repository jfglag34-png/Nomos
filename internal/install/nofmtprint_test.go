package install

// Инвариант пакета: internal/install не печатает сам — весь прогресс уходит в
// переданный io.Writer (nil = отбросить). Иначе HTTP-эндпоинт, вызвавший Run,
// рискует писать в чужой stdout, а тест установки не может прочитать, что
// произошло.
//
// Проверка читает исходники самого пакета через go/ast, как это уже сделано
// для кодов ошибок в internal/errdoc: регулярка откатилась бы молча на
// переименовании или на формате, а AST показывает именно вызов.
//
// Тесты ниже идут в обратном порядке важности: сначала фикстуры, которые
// показывают, что проверка ЛОВИТ нарушение (иначе она может быть зелёной просто
// потому, что ничего не проверяет), и только потом — настоящий пакет.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// findPrintCalls — нарушения печати в одном исходнике.
//
// Ловим четыре способа обойти запрет:
//   - fmt.Print/Printf/Println (в том числе через точку на алиасе импорта);
//   - os.Stdout / os.Stderr — печать мимо переданного writer;
//   - встроенные print/println;
//   - импорт "fmt" под другим именем: иначе проверка выше обходится
//     переименованием импорта одной строкой.
func findPrintCalls(path string) ([]string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	// Комментарии не разбираются: упоминание fmt.Print в пояснении не должно
	// считаться нарушением — иначе этот файл нельзя будет описать.
	f, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, err
	}
	alias := ""
	for _, imp := range f.Imports {
		if strings.Trim(imp.Path.Value, `"`) != "fmt" {
			continue
		}
		if imp.Name == nil {
			alias = "fmt"
		} else if imp.Name.Name == "_" || imp.Name.Name == "." {
			alias = "."
		} else {
			alias = imp.Name.Name
		}
	}
	var out []string
	report := func(n ast.Node, what string) {
		out = append(out, fset.Position(n.Pos()).String()+": "+what)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			ident, ok := x.X.(*ast.Ident)
			if !ok {
				return true
			}
			if alias != "" && ident.Name == alias && strings.HasPrefix(x.Sel.Name, "Print") {
				report(n, alias+"."+x.Sel.Name)
			}
			if ident.Name == "os" && (x.Sel.Name == "Stdout" || x.Sel.Name == "Stderr") {
				report(n, "os."+x.Sel.Name)
			}
		case *ast.CallExpr:
			if ident, ok := x.Fun.(*ast.Ident); ok && (ident.Name == "print" || ident.Name == "println") {
				report(n, ident.Name)
			}
		}
		return true
	})
	return out, nil
}

// packageSources — все .go файлы пакета, включая тесты. Каталог берём от
// исходника теста: `go test` запускает пакет в его каталоге, но полагаться на
// это нельзя — проверка молча прошла бы вхолостую по другому cwd.
func packageSources(t *testing.T) (string, []string) {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Skip("не удалось определить путь к исходнику")
	}
	dir := filepath.Dir(self)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		files = append(files, filepath.Join(dir, e.Name()))
	}
	return dir, files
}

// НАСТОЯЩИЙ пакет. Если этот тест падает — печать вернулась (и это правда).
// Если он падает потому, что файлов не нашлось, — сломана проверка (и это тоже
// правда, но чинить надо её).
func TestPackageDoesNotPrintItself(t *testing.T) {
	dir, files := packageSources(t)
	if len(files) == 0 {
		t.Fatalf("в %s не нашлось ни одного .go — проверка сломана, а не прошла", dir)
	}
	for _, path := range files {
		bad, err := findPrintCalls(path)
		if err != nil {
			t.Fatalf("разобрать %s: %v", filepath.Base(path), err)
		}
		for _, b := range bad {
			t.Errorf("internal/install печатает сам: %s", b)
		}
	}
}

// Фикстуры самой проверки: показывают, что нарушение находится. Без них тест
// выше может быть зелёным потому, что сканирует не то.
func TestFindPrintCallsCatchesViolations(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "fmt.Println",
			src:  "package p\n\nimport \"fmt\"\n\nfunc f() { fmt.Println(\"установлено\") }\n",
			want: "fmt.Println",
		},
		{
			name: "fmt.Printf",
			src:  "package p\n\nimport \"fmt\"\n\nfunc f() { fmt.Printf(\"%s\\n\", \"x\") }\n",
			want: "fmt.Printf",
		},
		{
			name: "fmt.Print под алиасом",
			src:  "package p\n\nimport f \"fmt\"\n\nfunc g() { f.Print(\"x\") }\n",
			want: "f.Print",
		},
		{
			name: "os.Stdout мимо writer",
			src:  "package p\n\nimport \"os\"\n\nfunc f() { os.Stdout.WriteString(\"x\") }\n",
			want: "os.Stdout",
		},
		{
			name: "встроенная println",
			src:  "package p\n\nfunc f() { println(\"x\") }\n",
			want: "println",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "p.go")
			if err := os.WriteFile(path, []byte(tt.src), 0o644); err != nil {
				t.Fatal(err)
			}
			bad, err := findPrintCalls(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(bad) != 1 || !strings.Contains(bad[0], tt.want) {
				t.Fatalf("нарушение не найдено: %v, ждали %q", bad, tt.want)
			}
		})
	}
}

// Обратная сторона: законная работа пакета нарушением не считается. Иначе
// проверку можно «починить», запретив fmt.Errorf, — и она станет зелёной на
// пустом месте.
func TestFindPrintCallsIgnoresLegalCode(t *testing.T) {
	src := "package p\n\nimport (\n\t\"fmt\"\n\t\"io\"\n)\n\n" +
		"// Пояснение для человека: fmt.Println здесь быть не должно.\n" +
		"var _ = fmt.Errorf(\"плагин %s не установлен\", \"x\")\n" +
		"var _ = fmt.Fprintf(io.Discard, \"  + %s\\n\", \"x\")\n" +
		"var printer = fmt.Sprint(\"x\")\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "p.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	bad, err := findPrintCalls(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 0 {
		t.Fatalf(" законный код помечен как печать: %v", bad)
	}
}
