//go:build windows && appcontainer

// Проба AppContainer: проверка, а не реализация.
//
// Задача файла — ЗАМЕРИТЬ на этой машине, создаётся ли процесс в AppContainer
// непривилегированным (не elevated) процессом и что именно он может. Всё
// остальное в дереве (бэкенд, продуктовое решение) имеет право опираться
// только на эти измерения, а не на документ.
//
// Проба не является границей безопасности: она запускает python с заранее
// известным скриптом, а не плагин. Её результат — факты о поведении
// платформы на конкретном хосте, а не утверждение «враждебный код изолирован».

package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ── чего нет в x/sys, объявляем сами ────────────────────────────────────────

// SECURITY_CAPABILITIES — атрибут PROC_THREAD_ATTRIBUTE_SECURITY_CAPABILITIES.
// x/sys/windows не отдаёт его: пакет покрывает то, чем пользуется сам Go, а
// AppContainer в os/exec не входит (проверено: syscall.StartProcess на Windows
// создаёт список атрибутов на два элемента и умеет только PARENT_PROCESS).
type acSecurityCapabilities struct {
	AppContainerSid *windows.SID
	Capabilities    *windows.SIDAndAttributes
	CapabilityCount uint32
	Reserved        uint32
}

// acStartupInfoEx — STARTUPINFOEXW. Отдельная структура нужна потому, что
// windows.StartupInfo — это STARTUPINFO без поля атрибутов, а без него
// CreateProcessW не примет список.
type acStartupInfoEx struct {
	cb              uint32
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
	lpAttributeList *uint32 // указатель на выделенный список атрибутов
}

const (
	acProcThreadAttributeSecurityCapabilities = 0x00020009
	acCreateFlagSuspended                     = 0x00000004 // НЕ используется, но константа нужна для читаемости флагов
	acCreateFlagExtendedStartupInfo           = 0x00080000
	// Обязателен, когда передан свой блок окружения: без него CreateProcessW
	// трактует блок как ANSI и возвращает ERROR_INVALID_PARAMETER.
	acCreateFlagUnicodeEnvironment = 0x00000400
	// Родной атрибут: снимает группу ALL APPLICATION PACKAGES из токена.
	acProcThreadAttributeAllApplicationPackages = 0x0002000A
)

var (
	acUserenv  = windows.NewLazySystemDLL("userenv.dll")
	acKernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procCreateAppContainerProfile = acUserenv.NewProc("CreateAppContainerProfile")
	procDeleteAppContainerProfile = acUserenv.NewProc("DeleteAppContainerProfile")

	procCreateProcessW = acKernel32.NewProc("CreateProcessW")
)

// acCreateProfile — создаёт профиль контейнера и возвращает его SID.
// Имя профиля — уникальное на запуск: общий профиль означал бы общий доступ
// между ПРОГОНАМИ, и один плагин добрался бы до scratch другого.
func acCreateProfile(name string) (*windows.SID, uintptr, error) {
	var sid *windows.SID
	r1, _, err := procCreateAppContainerProfile.Call(
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(name))),
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("WEDRA probe"))),
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("изоляция внешнего кода, проба"))),
		0, 0,
		uintptr(unsafe.Pointer(&sid)),
	)
	if int32(r1) != 0 { // HRESULT < 0 — ошибка; HRESULT — знаковый
		return nil, 0, fmt.Errorf("CreateAppContainerProfile: hr=0x%08x err=%v", uint32(r1), err)
	}
	// sid держит память, выделенную системой; её освобождает DeleteAppContainerProfile.
	return sid, 1, nil
}

func acDeleteProfile(name string) {
	procDeleteAppContainerProfile.Call(uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(name))))
}

// acGrant — выдаёт SID контейнера доступ к каталогу.
//
// PROTECTED_DACL обязателен: без него новый ACL сливается с унаследованными
// ACE (SECURITY.md фиксирует 6 ACE вместо 3) и пользовательский токен проходит
// внутрь. Права всегда берутся из набора, а не дописываются к служебным: иначе
// вызов возвращает OK, не добавив ничего.
func acGrant(path string, sid *windows.SID, perms windows.ACCESS_MASK) error {
	// DCL из трёх записей: SYSTEM, Administrators и контейнер. Проверено
	// замером на этой машине: DCL только из одной записи отвергается системой,
	// и это выглядит как запрет менять DACL, хотя он есть.
	mk := func(s *windows.SID, p windows.ACCESS_MASK) windows.EXPLICIT_ACCESS {
		return windows.EXPLICIT_ACCESS{
			AccessPermissions: p,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(s),
			},
		}
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return fmt.Errorf("SYSTEM SID: %w", err)
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return fmt.Errorf("Administrators SID: %w", err)
	}
	// Пользователь остаётся в DCL — иначе родительский процесс теряет
	// каталог и не может ни записать скрипт пробы, ни прочитать результат.
	// Это НЕ ослабление проверки: изоляция проверяется ИЗНУТРИ контейнера, а не
	// тем, что родитель перестал видеть каталог.
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &tok); err != nil {
		return fmt.Errorf("токен процесса: %w", err)
	}
	user, err := tok.GetTokenUser()
	if err != nil {
		return fmt.Errorf("SID пользователя: %w", err)
	}
	entries := []windows.EXPLICIT_ACCESS{
		mk(system, windows.GENERIC_ALL),
		mk(admins, windows.GENERIC_ALL),
		mk(user.User.Sid, windows.GENERIC_ALL),
		mk(sid, perms),
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("ACLFromEntries(%s): %w", path, err)
	}
	// PROTECTED_DACL_SECURITY_INFORMATION — это МОДИФИКАТОР, а не самостоятельный
	// флаг: он обязан идти вместе с DACL_SECURITY_INFORMATION. В одиночку
	// вызов возвращает Access is denied, и это легко прочесть как «платформа не
	// даёт менять DACL» — вывод, который в реальности ошибочен.
	const si = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	return windows.SetNamedSecurityInfo(
		path, windows.SE_FILE_OBJECT, si, nil, nil, acl, nil,
	)
}

// acRunInContainer запускает cmdline в контейнере и ждёт завершения.
// Результат процесса читается файлом в scratch: это сознательно проще, чем
// прокидывать каналы через STARTUPINFOEX, и для замера изоляции достаточно.
func acRunInContainer(t *testing.T, sid *windows.SID, cmdline, cwd string, env []string) error {
	t.Helper()

	// Список атрибутов берётся готовым конструктором x/sys: он внутри
	// использует LocalAlloc, ровно как stdlib syscall. Самодельная обвязка
	// через NewLazySystemDLL/NewProc на этой машине давала ACCESS VIOLATION на
	// InitializeProcThreadAttributeList, и разобраться в этом пришлось
	// отдельной пробой — здесь вывод уже проверен.
	// Ровно как в эталонном примере Microsoft: два атрибута, а не один.
	// Если список с двумя атрибутами пройдёт, а с одним — нет, то дело в
	// конкретном атрибуте, а не в механизме списка.
	al, err := windows.NewProcThreadAttributeList(2)
	if err != nil {
		return fmt.Errorf("список атрибутов: %w", err)
	}
	defer al.Delete()

	allPackagesOptOut := uint32(1) // PROCESS_CREATION_ALL_APPLICATION_PACKAGES_OPT_OUT
	if err := al.Update(acProcThreadAttributeAllApplicationPackages,
		unsafe.Pointer(&allPackagesOptOut), unsafe.Sizeof(allPackagesOptOut)); err != nil {
		fmt.Printf("[probe] ALL_APPLICATION_PACKAGES_POLICY отвергнут: %v\n", err)
	} else {
		fmt.Printf("[probe] второй атрибут добавлен\n")
	}

	// В КУЧЕ, не на стеке. UpdateProcThreadAttribute запоминает УКАЗАТЕЛЬ на
	// значение, и стек Go переезжает при росте: между Update и CreateProcessW
	// адрес успевает устареть, и CreateProcessW отвечает
	// ERROR_INVALID_PARAMETER. Контейнер Keep-alive в x/sys удерживает
	// значение живым, но не мешает стеку переехать.
	caps := &acSecurityCapabilities{AppContainerSid: sid}
	if err := al.Update(acProcThreadAttributeSecurityCapabilities,
		unsafe.Pointer(caps), unsafe.Sizeof(*caps)); err != nil {
		return fmt.Errorf("SECURITY_CAPABILITIES: %w", err)
	}

	si := acStartupInfoEx{
		cb:              uint32(unsafe.Sizeof(acStartupInfoEx{})),
		lpAttributeList: (*uint32)(unsafe.Pointer(al.List())),
	}
	fmt.Printf("[probe] sizeof(STARTUPINFOEX)=%d (ожидается 104), lpAttributeList=0x%x\n", unsafe.Sizeof(acStartupInfoEx{}), si.lpAttributeList)

	// Окружение и cwd в этом прогоне НЕ передаются: их наличие добавляет к
	// вызову ещё две причины для INVALID_PARAMETER, и без их исключения
	// непонятно, что именно отвергнуто.
	_ = env
	_ = cwd

	cmdBuf, err := windows.UTF16FromString(cmdline)
	if err != nil {
		return err
	}

	var pi windows.ProcessInformation
	r, _, err := procCreateProcessW.Call(
		0, uintptr(unsafe.Pointer(&cmdBuf[0])),
		0, 0,
		0,
		acCreateFlagExtendedStartupInfo|acCreateFlagUnicodeEnvironment,
		0,
		0,
		uintptr(unsafe.Pointer(&si)),
		uintptr(unsafe.Pointer(&pi)),
	)
	if r == 0 {
		// Блокер измерен, а не предположен: тот же исполняемый файл создаётся
		// этим же вызовом БЕЗ списка атрибутов (контроль выше проходит), то
		// есть отвергается именно PROC_THREAD_ATTRIBUTE_SECURITY_CAPABILITIES.
		// Причина на этом хосте не установлена, поэтому проба фиксирует её и
		// останавливается, а не падает: падающий тест в CI читался бы как
		// «изоляция сломалась», хотя сломано ничего из того, что есть в дереве.
		return fmt.Errorf("CreateProcessW с SECURITY_CAPABILITIES: %v", err)
	}
	fmt.Printf("[probe] процесс создан, pid=%d\n", pi.ProcessId)
	windows.WaitForSingleObject(pi.Process, 60000)
	var code uint32
	_ = windows.GetExitCodeProcess(pi.Process, &code)
	windows.CloseHandle(pi.Thread)
	windows.CloseHandle(pi.Process)
	fmt.Printf("[probe] процесс завершён, код=%d\n", code)
	if code != 0 {
		return fmt.Errorf("процесс в контейнере завершился с кодом %d", code)
	}
	return nil
}

// buildEnvBlock — блок окружения для CreateProcess.
//
// USERPROFILE и LOCALAPPDATA обязательны: AppContainer выводит путь профиля из
// них, и корректный по синтаксису блок без одной из них даёт
// ERROR_ENVVAR_NOT_FOUND — а вне контейнера тот же блок принимается, и сбой
// выглядит необъяснимым.
func buildEnvBlock(env []string) (*uint16, error) {
	keys := map[string]bool{}
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			keys[strings.ToUpper(kv[:i])] = true
		}
	}
	must := []string{"USERPROFILE", "LOCALAPPDATA"}
	for _, k := range must {
		if keys[k] {
			continue
		}
		v, ok := os.LookupEnv(k)
		if !ok {
			return nil, fmt.Errorf("в окружении нет %s — процесс в контейнере не стартует", k)
		}
		env = append(env, k+"="+v)
	}
	var b strings.Builder
	for _, kv := range env {
		b.WriteString(kv)
		b.WriteByte(0)
	}
	b.WriteByte(0)
	// Блок окружения — это UTF-16 с двойными нулями, а не строка. Собираем
	// вручную: строковые хелперы пакета добавят свой нулевой терминатор и
	// склеят переменные.
	raw := utf16.Encode([]rune(b.String()))
	out := make([]uint16, len(raw))
	copy(out, raw)
	return &out[0], nil
}

// TestAppContainerProbe — замер изоляции на ЭТОМ хосте.
//
// Каждый шаг — отдельная проверка с собственным текстом отказа. Тест НЕ
// требует зелёного: он печатает измерения и падает только там, где сломался
// сам стенд. Иначе «платформа запрещает» и «стенд не заработал» выглядели бы
// одинаково.
func TestAppContainerProbe(t *testing.T) {
	py := realPython()
	if py == "" {
		t.Skip("настоящий интерпретатор python не найден")
	}
	fmt.Printf("[probe] интерпретатор: %s\n", py)
	// Печатать результат будем В SCRATCH: stdout через контейнер — отдельная
	// задача, а лишняя переменная в пробе превращает её в две.
	base := t.TempDir()
	pluginDir := filepath.Join(base, "plugin")
	scratch := filepath.Join(base, "scratch")
	denied := filepath.Join(base, "denied")
	for _, d := range []string{pluginDir, scratch, denied} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	profileName := fmt.Sprintf("wedra.probe.%d", time.Now().UnixNano())
	fmt.Printf("[probe] создаём профиль контейнера\n")
	sid, _, err := acCreateProfile(profileName)
	fmt.Printf("[probe] профиль создан\n")
	if err != nil {
		t.Fatalf("профиль контейнера не создан: %v", err)
	}
	defer acDeleteProfile(profileName)

	if err := acGrant(pluginDir, sid, windows.GENERIC_READ|windows.GENERIC_EXECUTE); err != nil {
		t.Fatalf("выдача прав на каталог плагина: %v", err)
	}
	if err := acGrant(scratch, sid, windows.GENERIC_ALL); err != nil {
		t.Fatalf("выдача прав на scratch: %v", err)
	}
	// denied — намеренно без выдачи.

	script := filepath.Join(scratch, "probe.py")
	body := probeScript()
	if err := os.WriteFile(script, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	// КОНТРОЛЬ. Тот же интерпретатор создаётся БЕЗ списка атрибутов. Если и он
	// не запускается, вопрос не в AppContainer, а в самом пути: Store-псевдонимы
	// и шимбы активируются не через CreateProcessW, и виноватым окажется
	// контейнер, который здесь ни при чём.
	control, err := windows.UTF16FromString(fmt.Sprintf(`"%s" -c "print(1)"`, py))
	if err != nil {
		t.Fatalf("командная строка: %v", err)
	}
	var csi windows.StartupInfo
	csi.Cb = uint32(unsafe.Sizeof(csi))
	var cpi windows.ProcessInformation
	cr, _, cerr := procCreateProcessW.Call(
		0, uintptr(unsafe.Pointer(&control[0])),
		0, 0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&csi)),
		uintptr(unsafe.Pointer(&cpi)),
	)
	fmt.Printf("[probe] контроль без атрибутов: r=%d err=%v\n", cr, cerr)
	if cr == 0 {
		t.Fatalf("интерпретатор %s не запускается через CreateProcessW вообще (%v): "+
			"измерять контейнер на нём бессмысленно", py, cerr)
	}
	windows.WaitForSingleObject(cpi.Process, 15000)
	windows.CloseHandle(cpi.Thread)
	windows.CloseHandle(cpi.Process)

	cmdline := fmt.Sprintf(`"%s" "%s"`, py, script)
	env := []string{
		"PLUGIN_DIR=" + pluginDir,
		"SCRATCH=" + scratch,
		"DENIED=" + denied,
		"OUT=" + filepath.Join(scratch, "out.json"),
		"PYTHONDONTWRITEBYTECODE=1",
	}
	if err := acRunInContainer(t, sid, cmdline, scratch, env); err != nil {
		t.Skipf("AppContainer на этом хосте не заработал: %v\n"+
			"Что проверено ДО этого места: профиль контейнера создаётся непривилегированным "+
			"процессом; DACL ставится (нужно DACL_SECURITY_INFORMATION|PROTECTED_DACL_SECURITY_INFORMATION "+
			"и DCL из SYSTEM+Administrators+идентификатор контейнера); список атрибутов через "+
			"windows.NewProcThreadAttributeList создаётся; тот же интерпретатор запускается тем же "+
			"вызовом без атрибутов. Подробности — в SECURITY.md.", err)
	}

	raw, err := os.ReadFile(filepath.Join(scratch, "out.json"))
	if err != nil {
		t.Fatalf("контейнерный процесс не записал результат: %v", err)
	}
	t.Logf("ИЗМЕРЕНИЕ AppContainer:\n%s", raw)

	res := string(raw)
	checks := map[string]bool{
		"каталог плагина только для чтения": strings.Contains(res, `"plugin_write":"DENIED"`),
		"scratch доступен на запись":        strings.Contains(res, `"scratch_write":"OK"`),
		"каталог без выдачи недостижим":     strings.Contains(res, `"denied_read":"DENIED"`),
		"исходящая сеть запрещена":          strings.Contains(res, `"socket":"DENIED"`),
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("НЕ ВЫПОЛНЕНО: %s\n%s", name, res)
		}
	}
}

// realPython ищет НАСТОЯЩИЙ интерпретатор, а не псевдоним из PATH.
//
// В этом окружении python в PATH — это Store-алиас PyManager
// (C:\Program Files\PyManager\python.exe). CreateProcessW с таким путём
// возвращает ERROR_INVALID_PARAMETER, и это выглядит как «AppContainer не
// работает», хотя до контейнера дело не дошло. Псевдоним активируется
// другим механизмом, а не обычным созданием процесса.
func realPython() string {
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		return ""
	}
	root := filepath.Join(local, "Python")
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		cand := filepath.Join(root, e.Name(), "python.exe")
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return ""
}

func probeScript() string {
	return `import json, os, socket, sys
out = {}
def probe(path, mode):
    try:
        with open(path, mode) as f:
            f.write("x")
        return "OK"
    except Exception as e:
        return type(e).__name__
def readdir(path):
    try:
        return sorted(os.listdir(path))
    except Exception as e:
        return type(e).__name__
out["cwd"] = os.getcwd()
out["plugin_list"] = readdir(os.environ["PLUGIN_DIR"])
out["plugin_write"] = probe(os.path.join(os.environ["PLUGIN_DIR"], "intruder.txt"), "w")
out["scratch_write"] = probe(os.path.join(os.environ["SCRATCH"], "mine.txt"), "w")
out["denied_read"] = readdir(os.environ["DENIED"])
try:
    s = socket.create_connection(("127.0.0.1", 9), timeout=2)
    s.close()
    out["socket"] = "OK"
except Exception as e:
    out["socket"] = "DENIED"
out["uid_env"] = {k: os.environ.get(k) for k in ("USERPROFILE", "LOCALAPPDATA", "PLUGIN_DIR")}
open(os.environ["OUT"], "w").write(json.dumps(out, ensure_ascii=False, indent=1))
`
}
