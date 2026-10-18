package importer

import (
	"slices"
	"strings"
	"testing"
)

// TestParseRequirement — таблица успехов и отказов. Отказы здесь важнее
// успехов: разбор зависимостей — это место, где тихая ошибка превращается в
// плагин, который не запускается у другого человека.
func TestParseRequirement(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string // подстрока, которую обязано содержать сообщение
		want    Requirement
	}{
		{
			name:  "только имя",
			input: "httpx",
			want:  Requirement{Raw: "httpx", Name: "httpx"},
		},
		{
			name:  "имя с дефисом нормализуется",
			input: "python-dotenv",
			want:  Requirement{Raw: "python-dotenv", Name: "python-dotenv"},
		},
		{
			name:  "подчёркивание и точка — то же имя по PEP 503",
			input: "Zope_Interface",
			want:  Requirement{Raw: "Zope_Interface", Name: "zope-interface"},
		},
		{
			name:  "точный пин",
			input: "httpx==0.27.0",
			want:  Requirement{Raw: "httpx==0.27.0", Name: "httpx", Constraint: "==0.27.0", Version: "0.27.0", IsPinned: true},
		},
		{
			name:  "пин с пробелом вокруг оператора",
			input: "httpx == 0.27.0",
			want:  Requirement{Raw: "httpx == 0.27.0", Name: "httpx", Constraint: "==0.27.0", Version: "0.27.0", IsPinned: true},
		},
		{
			name:  "диапазон — не пин",
			input: "telethon>=1.36,<2.0",
			want:  Requirement{Raw: "telethon>=1.36,<2.0", Name: "telethon", Constraint: ">=1.36,<2.0"},
		},
		{
			name:  "только нижняя граница",
			input: "click>=8.1",
			want:  Requirement{Raw: "click>=8.1", Name: "click", Constraint: ">=8.1"},
		},
		{
			name:  "совместимый оператор ~=",
			input: "requests~=2.31",
			want:  Requirement{Raw: "requests~=2.31", Name: "requests", Constraint: "~=2.31"},
		},
		{
			name:  "неравенство и диапазон вместе",
			input: "pytest!=8.1.0,>=8.0",
			want:  Requirement{Raw: "pytest!=8.1.0,>=8.0", Name: "pytest", Constraint: "!=8.1.0,>=8.0"},
		},
		{
			name:  "pin с wildcard — не pin",
			input: "boto3==1.34.*",
			want:  Requirement{Raw: "boto3==1.34.*", Name: "boto3", Constraint: "==1.34.*"},
		},
		{
			name:    "две версии подряд — не pin",
			input:   "foo==1.0,==2.0",
			wantErr: "два оператора ==",
			want:    Requirement{Raw: "foo==1.0,==2.0", Name: "foo", Constraint: "==1.0,==2.0"},
		},
		{
			name:  "экстра-функция с диапазоном",
			input: "sqlalchemy[asyncio]>=2.0,<3.0",
			want: Requirement{
				Raw: "sqlalchemy[asyncio]>=2.0,<3.0", Name: "sqlalchemy",
				Extras: []string{"asyncio"}, Constraint: ">=2.0,<3.0",
			},
		},
		{
			name:  "несколько экстра-функций нормализуются",
			input: "pkg[Speedups,Security-Fix]",
			want: Requirement{
				Raw: "pkg[Speedups,Security-Fix]", Name: "pkg",
				Extras: []string{"speedups", "security_fix"},
			},
		},
		{
			name:  "ссылка на источник",
			input: "mypkg @ https://example.invalid/mypkg-0.1.tar.gz",
			want: Requirement{
				Raw: "mypkg @ https://example.invalid/mypkg-0.1.tar.gz", Name: "mypkg",
				URL: "https://example.invalid/mypkg-0.1.tar.gz",
			},
		},
		{
			name:  "маркер окружения сохраняется текстом",
			input: "tomli>=2.0; python_version < \"3.11\"",
			want: Requirement{
				Raw: "tomli>=2.0; python_version < \"3.11\"", Name: "tomli",
				Constraint: ">=2.0", Marker: "python_version < \"3.11\"",
			},
		},
		{
			name:  "комментарий в конце строки режется",
			input: "httpx==0.27.0  # закреплено руками",
			want:  Requirement{Raw: "httpx==0.27.0  # закреплено руками", Name: "httpx", Constraint: "==0.27.0", Version: "0.27.0", IsPinned: true},
		},
		{
			name:    "мусор: пустая строка",
			input:   "   ",
			wantErr: "пустое требование",
		},
		{
			name:    "мусор: только ограничение",
			input:   ">=1.0",
			wantErr: "не похоже на имя пакета",
		},
		{
			name:    "мусор: имя не начинается с буквы или цифры",
			input:   "-weird==1.0",
			wantErr: "не похоже на имя пакета",
		},
		{
			name:    "мусор: скобка без закрытия",
			input:   "pkg[extra",
			wantErr: "без закрывающей",
		},
		{
			name:    "мусор: пустая экстра-функция",
			input:   "pkg[]",
			wantErr: "пустое имя экстра-функции",
		},
		{
			name:    "мусор: неизвестный оператор",
			input:   "pkg=>=1.0",
			wantErr: "не понимаю ограничение",
		},
		{
			name:    "мусор: пустая часть ограничения",
			input:   "pkg>=1.0,",
			wantErr: "пустая часть ограничения",
		},
		{
			name:    "мусор: маркер без текста",
			input:   "pkg>=1.0;",
			wantErr: "маркер пустой",
		},
		{
			name:    "мусор: ссылка без адреса",
			input:   "pkg @",
			wantErr: "после '@' пустая ссылка",
		},
		{
			name:    "мусор: ссылка с пробелом",
			input:   "pkg @ https://example.invalid/a b.tar.gz",
			wantErr: "пробелы",
		},
		{
			name:    "мусор: строка целиком",
			input:   "git+https://example.invalid/repo@main#egg=pkg",
			wantErr: "прямой ссылкой на репозиторий",
		},
		{
			name:    "мусор: ссылка на файл requirements",
			input:   "-r base.txt",
			wantErr: "не похоже на имя пакета",
		},
		{
			name:    "мусор: правка пакета",
			input:   "-e .",
			wantErr: "не похоже на имя пакета",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseRequirement(tc.input)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ожидалась ошибка %q, а разбор прошёл: %+v", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ошибка %q не содержит %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("неожиданная ошибка: %v", err)
			}
			if got.Name != tc.want.Name {
				t.Errorf("Name = %q, ждали %q", got.Name, tc.want.Name)
			}
			if got.Constraint != tc.want.Constraint {
				t.Errorf("Constraint = %q, ждали %q", got.Constraint, tc.want.Constraint)
			}
			if got.Version != tc.want.Version {
				t.Errorf("Version = %q, ждали %q", got.Version, tc.want.Version)
			}
			if got.IsPinned != tc.want.IsPinned {
				t.Errorf("IsPinned = %v, ждали %v", got.IsPinned, tc.want.IsPinned)
			}
			if got.URL != tc.want.URL {
				t.Errorf("URL = %q, ждали %q", got.URL, tc.want.URL)
			}
			if got.Marker != tc.want.Marker {
				t.Errorf("Marker = %q, ждали %q", got.Marker, tc.want.Marker)
			}
			if !slices.Equal(got.Extras, tc.want.Extras) {
				t.Errorf("Extras = %v, ждали %v", got.Extras, tc.want.Extras)
			}
		})
	}
}

// TestParseRequirementNeverInventsVersion — отдельный тест на главное
// правило: диапазон не превращается в пин. Если это сломается, в манифест
// попадёт версия, которую никто не выбирал.
func TestParseRequirementNeverInventsVersion(t *testing.T) {
	for _, input := range []string{"telethon>=1.36,<2.0", "requests>=2.31", "x~=1.4", "y", "z==1.2.*"} {
		req, err := ParseRequirement(input)
		if err != nil {
			t.Fatalf("%q: неожиданная ошибка %v", input, err)
		}
		if req.IsPinned {
			t.Errorf("%q: стал пин %q — версия выдумана", input, req.Version)
		}
		if req.Version != "" {
			t.Errorf("%q: Version = %q, а пина нет", input, req.Version)
		}
		if _, ok := req.ManifestSpec(); ok {
			t.Errorf("%q: ManifestSpec дал строку для манифеста без точного пина", input)
		}
	}
}

// TestManifestSpecFollowsSchema — ManifestSpec обязан совпадать с шаблоном из
// schemas/manifest.schema.json, иначе плагин не пройдёт валидацию уже после
// импорта.
func TestManifestSpecFollowsSchema(t *testing.T) {
	tests := []struct {
		input string
		want  string
		ok    bool
	}{
		{input: "httpx==0.27.0", want: "httpx==0.27.0", ok: true},
		{input: "python-dotenv==1.0.1", want: "python-dotenv==1.0.1", ok: true},
		{input: "pkg==2.0.0rc1", want: "pkg==2.0.0rc1", ok: true},
		// экстра-функция: пин есть, но шаблон манифеста скобок не допускает
		{input: "sqlalchemy[asyncio]==2.0.30"},
		// маркер окружения: версия есть, условие в манифесте выразить нечем
		{input: "tomli==2.0.1; python_version < \"3.11\""},
		// эпоха PEP 440: версия честная, но символа `!` в шаблоне нет
		{input: "legacy==1!2.0"},
	}
	for _, tc := range tests {
		req, err := ParseRequirement(tc.input)
		if err != nil {
			t.Fatalf("%q: неожиданная ошибка %v", tc.input, err)
		}
		spec, ok := req.ManifestSpec()
		if ok != tc.ok {
			t.Fatalf("%q: ManifestSpec ok = %v, ждали %v (spec %q)", tc.input, ok, tc.ok, spec)
		}
		if ok {
			if spec != tc.want {
				t.Errorf("%q: spec = %q, ждали %q", tc.input, spec, tc.want)
			}
			if !manifestRequiresRe.MatchString(spec) {
				t.Errorf("%q: %q не проходит шаблон манифеста", tc.input, spec)
			}
		}
	}
}

// TestNormalizeNames — нормализация имён обязана совпадать с PEP 503/PEP 685,
// иначе один и тот же пакет будет записан в план двумя строками.
func TestNormalizeNames(t *testing.T) {
	cases := map[string]string{
		"Python-Dotenv":  "python-dotenv",
		"python_dotenv":  "python-dotenv",
		"A.B":            "a-b",
		"zope.interface": "zope-interface",
		"  httpx  ":      "httpx",
	}
	for input, want := range cases {
		if got := normalizeDistName(input); got != want {
			t.Errorf("normalizeDistName(%q) = %q, ждали %q", input, got, want)
		}
	}
	if got := normalizeExtra("Security-Fix"); got != "security_fix" {
		t.Errorf("normalizeExtra = %q, ждали security_fix", got)
	}
}
