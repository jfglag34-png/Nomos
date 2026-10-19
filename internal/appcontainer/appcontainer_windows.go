//go:build windows

package appcontainer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Точки входа AppContainer живут в userenv.dll и НЕ обёрнуты в x/sys/windows:
// проверено, в пакете нет ни CreateAppContainerProfile, ни
// DeriveAppContainerSidFromAppContainerName. Поэтому здесь ручные привязки.
//
// Отдельно про ручные привязки: самодельный
// NewLazySystemDLL(...).NewProc("InitializeProcThreadAttributeList").Call(...)
// на этой машине ронял процесс ACCESS VIOLATION, хотя тот же вызов через
// windows.NewProcThreadAttributeList отрабатывает. Для списка атрибутов поэтому
// используется только обёртка x/sys. Функции ниже берутся через LazyProc и
// проверены измерением: профиль создаётся, а процесс в контейнере создаётся.

var (
	userenv                   = windows.NewLazySystemDLL("userenv.dll")
	procCreateAppContainer    = userenv.NewProc("CreateAppContainerProfile")
	procDeleteAppContainer    = userenv.NewProc("DeleteAppContainerProfile")
	procDeriveAppContainerSID = userenv.NewProc("DeriveAppContainerSidFromAppContainerName")
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	procCreateProcess         = kernel32.NewProc("CreateProcessW")
)

// HRESULT, которые приходится различать вручную: готовых констант в x/sys нет.
const (
	hresultAlreadyExists = 0x800700B7 // HRESULT_FROM_WIN32(ERROR_ALREADY_EXISTS)
	// CreateFlagExtendedStartupInfo — без него STARTUPINFOEX не читается.
	createFlagExtendedStartupInfo = 0x00080000
	createFlagUnicodeEnvironment  = 0x00000400
	startfUseStdHandles           = 0x00000100
)

// startupInfoEx — зеркало STARTUPINFOEXW.
//
// Поле lpReserved обязано здесь быть: в STARTUPINFOW оно стоит между cb и
// lpDesktop, и без него всё последующее сдвигается на 8 байт. Тогда
// lpAttributeList оказывается по смещению 96 вместо 104, при этом sizeof даёт
// 104 — корректный размер STARTUPINFOW, поэтому cb CreateProcessW не
// отвергает, но с EXTENDED_STARTUPINFO_PRESENT он читает lpAttributeList за
// пределами структуры и отвечает ERROR_INVALID_PARAMETER. Это стоило дня
// расследований; смещения сверены с нативным эталоном на C.
type startupInfoEx struct {
	cb              uint32
	lpReserved      *uint16
	lpDesktop       *uint16
	lpTitle         *uint16
	dwX             uint32
	dwY             uint32
	dwXSize         uint32
	dwYSize         uint32
	dwXCountChars   uint32
	dwYCountChars   uint32
	dwFillAttribute uint32
	dwFlags         uint32
	wShowWindow     uint16
	cbReserved2     uint16
	lpReserved2     *byte
	hStdInput       windows.Handle
	hStdOutput      windows.Handle
	hStdErr         windows.Handle
	lpAttributeList *uint32
}

// securityCapabilities — зеркало SECURITY_CAPABILITIES.
//
// Capabilities обязан быть УКАЗАТЕЛЕМ на SID_AND_ATTRIBUTES, а не структурой по
// значению: ровно эта ошибка описана как причина ERROR_INVALID_PARAMETER у
// людей, которые доходят до этого вызова. Здесь Capabilities = NULL при
// CapabilityCount = 0, то есть у контейнера НОЛЬ сетевых прав, и сеть ему не
// выдаётся. CapabilityCount = 0 — допустимое значение, а не недосыл:
// так создаётся контейнер без сети и в Chromium.
type securityCapabilities struct {
	appContainerSID *windows.SID
	capabilities    *windows.SIDAndAttributes
	capabilityCount uint32
	reserved        uint32
}

const procThreadAttributeSecurityCapabilities = 0x00020009

// hresultFailed — HRESULT считается неуспешным, если старшие 16 бит установлены.
func hresultFailed(hr uintptr) bool { return int32(hr) < 0 }

// profileSID — SID профиля контейнера, с созданием профиля при первом обращении.
//
// Порядок именно «сначала создать, потом вывести», и это не стилистика:
// DeriveAppContainerSidFromAppContainerName умеет вычислить SID и для профиля,
// которого ещё нет, и возвращает успех. Если начинать с него, профиль не
// создаётся вовсе, SID оказывается от несуществующего контейнера, и
// CreateProcessW отвечает ERROR_FILE_NOT_FOUND — на существующий интерпретатор,
// с корректными правами и верной командной строкой.
//
// Профиль на плагин, а не на запуск, поэтому SID переиспользуется между
// прогонами, а выданная однажды ACE остаётся рабочей.
func profileSID(name string) (*windows.SID, error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("имя профиля %q: %w", name, err)
	}
	// DisplayName и описание на английском: они показываются в диспетчере
	// приложений, где русский текст неуместен.
	display, _ := windows.UTF16PtrFromString("WEDRA plugin")
	desc, _ := windows.UTF16PtrFromString("Изоляция плагина WEDRA")
	var sid *windows.SID
	hr, _, _ := procCreateAppContainer.Call(
		uintptr(unsafe.Pointer(namePtr)),
		uintptr(unsafe.Pointer(display)),
		uintptr(unsafe.Pointer(desc)),
		0, // pCapabilities — у контейнера нет заявленных прав
		0, // dwCapabilityCount
		uintptr(unsafe.Pointer(&sid)),
	)
	if !hresultFailed(hr) && sid != nil {
		return sid, nil
	}
	// Профиль уже был создан — SID выводим по имени.
	hr, _, _ = procDeriveAppContainerSID.Call(
		uintptr(unsafe.Pointer(namePtr)),
		uintptr(unsafe.Pointer(&sid)),
	)
	if hresultFailed(hr) || sid == nil {
		return nil, fmt.Errorf("%w: CreateAppContainerProfile(%s) вернул 0x%08X, "+
			"и DeriveAppContainerSidFromAppContainerName не спас (0x%08X)",
			ErrHostRefused, name, uint32(hr), uint32(hr))
	}
	return sid, nil
}

// grantReadOnly — выдаёт контейнеру чтение и исполнение на путь.
//
// Слияние с СУЩЕСТВУЮЩИМ DCL, а не пересборка ACL с нуля: пересборка с
// PROTECTED заменила бы права на настоящей установке интерпретатора и сломала
// её. Наследование не отключается, поэтому флаг PROTECTED здесь не
// используется намеренно.
//
// Выдача идемпотентна: повторный вызов с тем же SID ничего не меняет, поэтому
// её безопасно повторять на каждом запуске, хотя профиль на плагин и создаётся
// один раз.
func grant(path string, sid *windows.SID, perms windows.ACCESS_MASK) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("путь %s: %w", path, err)
	}
	if _, err := os.Stat(abs); err != nil {
		return fmt.Errorf("путь для выдачи прав недоступен: %w", err)
	}
	existing, err := windows.GetNamedSecurityInfo(abs, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("чтение DCL(%s): %w", abs, err)
	}
	old, _, err := existing.DACL()
	if err != nil {
		return fmt.Errorf("DCL(%s): %w", abs, err)
	}
	entry := windows.EXPLICIT_ACCESS{
		AccessPermissions: perms,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
	merged, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{entry}, old)
	if err != nil {
		return fmt.Errorf("слияние ACL(%s): %w", abs, err)
	}
	if err := windows.SetNamedSecurityInfo(abs, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION, nil, nil, merged, nil); err != nil {
		return fmt.Errorf("выдача прав контейнеру(%s): %w", abs, err)
	}
	return nil
}

// longPath разворачивает путь до канонического вида: 8.3-имя разворачивается в
// длинное, симлинки и сокращения разрешаются.
//
// Это не косметика, а условие работоспособности. Один и тот же каталог на этой
// машине доступен и под длинным именем профиля (с кириллицей), и под
// коротким 8.3-именем. Если интерпретатор приходит длинным путём, а scratch —
// коротким, CreateProcessW не сводит их в один путь и отвечает
// ERROR_FILE_NOT_FOUND, хотя оба пути верны и оба существуют. Наблюдалось
// ровно так: в команде был длинный путь к python.exe и короткий к рабочему
// каталогу.
func longPath(p string) string {
	if p == "" {
		return ""
	}
	in, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return p
	}
	const probe = 512
	buf := make([]uint16, probe)
	n, err := windows.GetLongPathName(in, &buf[0], probe)
	if err != nil || n == 0 || int(n) >= probe {
		// Длиннее буфера: лучше не развернуть вовсе, чем вернуть обрезанный путь.
		return p
	}
	return windows.UTF16ToString(buf[:n])
}

// normalize приводит план к каноническим путям ДО операций с файловой системой и
// ДО создания процесса.
//
// Разворачиваются ВСЕ аргументы, а не только исполняемый файл: путь к скрипту
// передаётся тоже аргументом, и оставленный в 8.3-виде он даёт тот же
// ERROR_FILE_NOT_FOUND. Для аргументов, которые не пути, GetLongPathName
// возвращает ошибку, и longPath оставляет их как есть.
func (p *Plan) normalize() {
	for i, a := range p.Argv {
		p.Argv[i] = longPath(a)
	}
	p.Scratch = longPath(p.Scratch)
	p.WorkDir = longPath(p.WorkDir)
	for i, r := range p.Read {
		p.Read[i] = longPath(r)
	}
}

// Prepare — создаёт профиль контейнера и выдаёт права. Идемпотентно.
//
// Вынесено отдельно от Run, чтобы ACL можно было подготовить один раз при
// установке плагина, а не платить за изменение DCL на каждом прогоне.
func Prepare(p Plan) error {
	if err := p.Validate(); err != nil {
		return err
	}
	p.normalize()
	sid, err := profileSID(p.Name)
	if err != nil {
		return err
	}
	// Сначала scratch, и с правами на запись: общая с read-путами выдача дала бы
	// плагину каталог, в который нельзя сохранить результат.
	if err := grant(p.Scratch, sid, windows.GENERIC_ALL); err != nil {
		return err
	}
	for _, path := range p.Read {
		if strings.TrimSpace(path) == "" {
			continue
		}
		if err := grant(path, sid, windows.GENERIC_READ|windows.GENERIC_EXECUTE); err != nil {
			return err
		}
	}
	return nil
}

// envBlock — блок окружения для CreateProcess: UTF-16, каждая переменная
// заканчивается NUL, весь блок — дополнительным NUL.
//
// Без USERPROFILE и LOCALAPPDATA AppContainer выводит путь профиля из
// переменных окружения, и корректный по синтаксису блок без одной из них даёт
// ERROR_ENVVAR_NOT_FOUND. Вне контейнера тот же блок принимается, поэтому сбой
// выглядит необъяснимым.
func envBlock(env []string) ([]uint16, error) {
	keys := map[string]bool{}
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			keys[strings.ToUpper(kv[:i])] = true
		}
	}
	for _, k := range []string{"USERPROFILE", "LOCALAPPDATA"} {
		if keys[k] {
			continue
		}
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	env = append(env, "PYTHONUTF8=1", "PYTHONDONTWRITEBYTECODE=1")
	// Собираем блок вручную, а не через windows.UTF16FromString: тот добавляет
	// СВОЙ нулевой терминатор, и лишний ноль на стыке превращается в
	// переменную с пустым именем. CreateProcessW на таком блоке отвечает
	// ERROR_ENVVAR_NOT_FOUND, и это выглядит как «AppContainer сломан», хотя
	// сломана одна склейка. Проверено измерением: хвост блока должен быть
	// РОВНО из двух нулей — закрытие последней переменной и закрытие блока.
	var b strings.Builder
	for _, kv := range env {
		b.WriteString(kv)
		b.WriteByte(0)
	}
	b.WriteByte(0)
	raw := utf16.Encode([]rune(b.String()))
	return raw, nil
}

// stdHandles — дескрипторы родителя. Контейнеризованный ребёнк наследует их
// напрямую, а не через цепочку каналов: иначе stdout плагина пришлось бы
// перекладывать вручную, а это ровно та обвязка, ради которой уход от exec.Cmd
// нежелателен.
//
// Ошибка здесь не фатальна: если дескриптор не выдался, поле остаётся нулевым, а
// CreateProcessW с STARTF_USESTDHANDLES и нулевым дескриптором отдаёт ребёнку
// недействительный handle — он не пишет, но и не падает. Это фиксится вызывающей
// стороной, которая знает, куда писать, поэтому молчание здесь уместно.
func stdHandles() (in, out, errh windows.Handle) {
	get := func(which uint32) windows.Handle {
		h, err := windows.GetStdHandle(which)
		if err != nil {
			return 0
		}
		return h
	}
	return get(windows.STD_INPUT_HANDLE),
		get(windows.STD_OUTPUT_HANDLE),
		get(windows.STD_ERROR_HANDLE)
}

// Run — создаёт профиль (если нужно), выдаёт права и запускает процесс в
// контейнере, дожидаясь его завершения.
func Run(ctx context.Context, p Plan) (Result, error) {
	if err := p.Validate(); err != nil {
		return Result{}, err
	}
	if err := Prepare(p); err != nil {
		return Result{}, err
	}
	p.normalize()
	sid, err := profileSID(p.Name)
	if err != nil {
		return Result{}, err
	}
	env, err := envBlock(p.Env)
	if err != nil {
		return Result{}, err
	}
	cmdLine, err := windows.UTF16FromString(quoteArgv(p.Argv))
	if err != nil {
		return Result{}, err
	}
	// NULL вместо пустой строки: CreateProcessW различает их, и пустая строка
	// означает «каталог с именем пустым», которого нет.
	var workDirPtr uintptr
	if p.WorkDir != "" {
		wp, werr := windows.UTF16PtrFromString(p.WorkDir)
		if werr != nil {
			return Result{}, werr
		}
		workDirPtr = uintptr(unsafe.Pointer(wp))
	}

	// Список атрибутов — только через обёртку x/sys, ручная привязка на этой
	// машине падала ACCESS VIOLATION.
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return Result{}, fmt.Errorf("список атрибутов: %w", err)
	}
	defer attrs.Delete()

	// В куче, не на стеке: UpdateProcThreadAttribute запоминает УКАЗАТЕЛЬ на
	// значение, а стек Go переезжает при росте, и между Update и CreateProcessW
	// адрес успевает устареть.
	caps := &securityCapabilities{appContainerSID: sid}
	if err := attrs.Update(procThreadAttributeSecurityCapabilities,
		unsafe.Pointer(caps), unsafe.Sizeof(*caps)); err != nil {
		return Result{}, fmt.Errorf("SECURITY_CAPABILITIES: %w", err)
	}

	stdIn, stdOut, stdErr := stdHandles()
	si := startupInfoEx{
		cb:              uint32(unsafe.Sizeof(startupInfoEx{})),
		lpAttributeList: (*uint32)(unsafe.Pointer(attrs.List())),
		dwFlags:         startfUseStdHandles,
		hStdInput:       stdIn,
		hStdOutput:      stdOut,
		hStdErr:         stdErr,
	}
	var pi windows.ProcessInformation

	r, _, cerr := procCreateProcess.Call(
		0, // lpApplicationName — путь берётся из командной строки
		uintptr(unsafe.Pointer(&cmdLine[0])),
		0, 0, // lpProcessAttributes, lpThreadAttributes
		1, // bInheritHandles — иначе выданные дескрипторы до ребёнка не дойдут
		createFlagExtendedStartupInfo|createFlagUnicodeEnvironment,
		uintptr(unsafe.Pointer(&env[0])),
		workDirPtr,
		uintptr(unsafe.Pointer(&si)),
		uintptr(unsafe.Pointer(&pi)),
	)
	if r == 0 {
		return Result{}, fmt.Errorf("%w: CreateProcessW в контейнере не создал процесс (%v); "+
			"команда=%q рабочий каталог=%q (существует: %v); выдано на чтение: %v",
			ErrHostRefused, cerr, quoteArgv(p.Argv), p.WorkDir, dirExists(p.WorkDir), p.Read)
	}
	defer windows.CloseHandle(pi.Thread)
	defer windows.CloseHandle(pi.Process)

	// Ожидание с учётом отмены контекста: внешний код не должен переживать
	// таймаут запуска.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = windows.WaitForSingleObject(pi.Process, windows.INFINITE)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		// Процесс не убивается здесь намеренно: его снимает Job Object
		// родителя, и убийство в обход джобы оставило бы сироту.
		<-done
	}
	var code uint32
	if err := windows.GetExitCodeProcess(pi.Process, &code); err != nil {
		return Result{}, fmt.Errorf("код возврата контейнеризованного процесса: %w", err)
	}
	return Result{ExitCode: int(int32(code))}, nil
}

// quoteArgv — склейка argv в командную строку CreateProcessW.
//
// Первый аргумент берётся в кавычки ВСЕГДА: путь к интерпретатору почти всегда
// содержит пробелы (имя пользователя), а без кавычек CreateProcessW ищет файл,
// которого нет, и отвечает ошибкой, похожей на отказ контейнера.
func quoteArgv(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, a := range argv {
		parts = append(parts, `"`+a+`"`)
	}
	return strings.Join(parts, " ")
}

// DeleteProfile — убирает профиль контейнера.
//
// Отдельная функция, потому что профиль на плагин переживает запуски и должен
// удаляться явно — либо при снятии плагина, либо при отладке вручную. В ходе
// обычной работы не вызывается.
func DeleteProfile(name string) error {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	hr, _, _ := procDeleteAppContainer.Call(uintptr(unsafe.Pointer(namePtr)))
	if hresultFailed(hr) && hr != hresultAlreadyExists {
		return fmt.Errorf("DeleteAppContainerProfile(%s): 0x%08X", name, uint32(hr))
	}
	return nil
}

// dirExists — для диагностики ошибки запуска, не для логики.
func dirExists(p string) bool {
	if p == "" {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// Probe — может ли хост в принципе создать AppContainer.
//
// Отдельная функция, потому что проверка «изолятор вообще возможен» не должна
// требовать интерпретатора: на CI-раннере Python живёт не там, где его ищет
// бэкенд, и проба с Python всегда давала бы ложный «изолятора нет», хотя
// ограничение платформы тут ни при чём.
//
// Проверка честная и минимальная: создать профиль, получить его SID и удалить.
// Никаких прав и процессов — этого достаточно, чтобы отличить «платформа умеет»
// от «политика хоста запрещает».
func Probe() error {
	name := ProfileName("probe." + strconv.Itoa(os.Getpid()))
	if _, err := profileSID(name); err != nil {
		return err
	}
	if err := DeleteProfile(name); err != nil {
		return fmt.Errorf("профиль создался, но не удалился: %w", err)
	}
	return nil
}
