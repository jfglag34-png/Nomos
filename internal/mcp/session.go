package mcp

import (
	"context"
	"encoding/json"
	"github.com/wykserdex/wedra/internal/buildinfo"
	"github.com/wykserdex/wedra/internal/pipeline"
	"strings"
	"sync"
)

func (s *Server) Serve(t *Transport) error {
	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	s.mu.Lock()
	s.transport = t
	s.mu.Unlock()
	for {
		frame, err := t.ReadFrame()
		if err != nil {
			wg.Wait()
			select {
			case writeErr := <-errCh:
				return writeErr
			default:
				return err
			}
		}
		if frame.Batch {
			// Порядок ответов батча обязан совпадать с порядком запросов
			// (JSON-RPC 2.0), поэтому батч идёт последовательно: параллелизм
			// внутри одного кадра не даёт ничего, кроме гонки за порядок.
			responses := make([]*Response, 0, len(frame.Items))
			for _, item := range frame.Items {
				if item.Err != nil {
					responses = append(responses, item.Err.response())
					continue
				}
				if item.Resp != nil {
					s.deliverClientResponse(item.Resp)
					continue
				}
				req := item.Request
				if isNotification(req) {
					s.handleNotification(req)
					continue
				}
				if len(req.ID) == 0 {
					continue
				}
				call := s.beginCall(t, req)
				res := s.handleCall(call, req)
				s.endCall(req)
				res.ID = req.ID
				responses = append(responses, res)
			}
			if len(responses) == 0 {
				continue // батч из одних нотификаций: отвечать нечем
			}
			if err := t.WriteBatch(responses); err != nil {
				return err
			}
			continue
		}
		item := frame.Items[0]
		if item.Err != nil {
			if err := t.Write(item.Err.response()); err != nil {
				return err
			}
			continue
		}
		if item.Resp != nil {
			// Ответ клиента на серверный запрос (elicitation): адресат ждёт его
			// на канале, ответа клиенту не требуется.
			s.deliverClientResponse(item.Resp)
			continue
		}
		req := item.Request
		if isNotification(req) {
			s.handleNotification(req)
			continue
		}
		if len(req.ID) == 0 {
			continue
		}
		call := s.beginCall(t, req)
		wg.Add(1)
		go func(req *Request, call *inflightCall) {
			defer wg.Done()
			defer s.endCall(req)
			res := s.handleCall(call, req)
			res.ID = req.ID
			if err := t.Write(res); err != nil {
				select {
				case errCh <- err:
				default:
				}
			}
		}(req, call)
	}
}

// requestKey — ключ сопоставления запроса и нотификации об отмене. JSON-RPC
// разрешает id и числом, и строкой; клиент возвращает ровно то значение,
// которое отправил, поэтому ключом служат сырые байты, а не разобранное число.
func requestKey(id json.RawMessage) string {
	return strings.TrimSpace(string(id))
}

// beginCall — зарегистрировать вызов до его исполнения. Регистрация нужна
// именно в этот момент, а не при старте рана: нотификация об отмене может
// прийти, пока инструмент ещё валидирует пайплайн, и это не ошибка клиента.
func (s *Server) beginCall(t *Transport, req *Request) *inflightCall {
	key := requestKey(req.ID)
	call := &inflightCall{key: key, writer: t}
	if key == "" {
		return call
	}
	s.mu.Lock()
	if s.inflight == nil {
		s.inflight = map[string]*inflightCall{}
	}
	s.inflight[key] = call
	s.mu.Unlock()
	return call
}

func (s *Server) endCall(req *Request) {
	key := requestKey(req.ID)
	if key == "" {
		return
	}
	s.mu.Lock()
	delete(s.inflight, key)
	s.mu.Unlock()
}

// bindRun — привязать к вызову запущенный им ран. Связь живёт, пока жив ран
// (s.runRequests), а не пока жив запрос: у MCP-клиента запрос заканчивается
// раньше рана.
func (s *Server) bindRun(call *inflightCall, runID string) {
	if call == nil || runID == "" {
		return
	}
	s.mu.Lock()
	call.runID = runID
	if call.key != "" {
		if s.runRequests == nil {
			s.runRequests = map[string]string{}
		}
		s.runRequests[call.key] = runID
	}
	late := call.cancelled
	if late {
		if st, ok := s.runs[runID]; ok {
			st.cancelRequested = true
		}
	}
	s.mu.Unlock()
	if late {
		// Отмена пришла раньше старта рана: без этой ветки нотификация
		// терялась бы, и ран доезжал бы до конца, а клиент — до таймаута.
		if rpcErr := s.cancelRun(runID); rpcErr != nil {
			s.logf("notifications/cancelled: ран %s отменить не удалось: %s", runID, rpcErr.Message)
		}
	}
}

// bindExec — привязать к вызову процесс exec_plugin.
func (s *Server) bindExec(call *inflightCall, cancel context.CancelFunc) {
	if call == nil || cancel == nil {
		return
	}
	s.mu.Lock()
	call.cancelExec = cancel
	late := call.cancelled
	s.mu.Unlock()
	if late {
		cancel()
	}
}

// handleNotification — нотификации жизненного цикла. Ответа не требуют
// (isNotification), но не все безопасны к игнорированию:
// notifications/cancelled означает «отмени работу запроса <id>». Раньше метод
// просто пропускался, и отменённый клиентом запрос продолжал держать
// единственный слот рана — агент до таймаута получал E_RUN_BUSY и не мог
// запустить ничего нового.
func (s *Server) handleNotification(req *Request) {
	if req.Method != "notifications/cancelled" {
		return
	}
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
		Reason    string          `json:"reason"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return
	}
	key := requestKey(p.RequestID)
	if key == "" {
		return
	}
	s.cancelInflight(key, p.Reason)
}

// cancelInflight — отмена по нотификации клиента.
//
// Ищутся два адресата, и порядок важен. Сначала живой вызов: его ран, если он
// есть, и запущенный процесс exec_plugin. Потом — ран, который пережил свой
// запрос (waiting_human или фоновый): буква спеки советует игнорировать
// нотификацию к завершённому запросу, но у нас «завершённый запрос» и
// «законченный ран» — разные вещи, и именно этот случай оставлял слот занятым
// до таймаута.
func (s *Server) cancelInflight(key, reason string) {
	s.mu.Lock()
	call := s.inflight[key]
	var runID string
	var cancelExec context.CancelFunc
	if call != nil {
		call.cancelled = true
		runID = call.runID
		cancelExec = call.cancelExec
	}
	if runID == "" {
		runID = s.runRequests[key]
	}
	if call == nil && runID == "" {
		s.mu.Unlock()
		s.logf("notifications/cancelled: запрос %s завершён и рана за ним нет — отменять нечего", key)
		return
	}
	s.mu.Unlock()
	if cancelExec != nil {
		cancelExec()
	}
	if runID == "" {
		// Инструмент ещё не дошёл до старта рана: отменять нечего, а признак
		// на вызове остался — bindRun отменит ран сразу после его появления.
		s.logf("notifications/cancelled: запрос %s отменён до старта рана (%s)", key, reason)
		return
	}
	if rpcErr := s.cancelRun(runID); rpcErr != nil {
		s.logf("notifications/cancelled: ран %s отменить не удалось: %s", runID, rpcErr.Message)
		return
	}
	s.logf("notifications/cancelled: ран %s отменён по запросу клиента (%s)", runID, reason)
}

// cancelRun — общая точка отмены: cancel_run и notifications/cancelled приходят
// сюда, чтобы «отменено» означало одно и то же независимо от того, кто просил.
func (s *Server) cancelRun(runID string) *RPCError {
	s.mu.Lock()
	st, known := s.runs[runID]
	cancel, hasCancel := s.cancels[runID]
	if !known {
		s.mu.Unlock()
		return rpcErr("", "ран не найден: "+runID)
	}
	if cancel == nil || !hasCancel {
		select {
		case <-st.done:
			s.mu.Unlock()
			return &RPCError{Code: -32000, Message: "ран уже завершён", Data: map[string]string{"code": "E_RUN_DONE"}}
		default:
			// cancel() регистрируется на горутине рана, то есть может ещё не
			// существовать. Запоминаем намерение: ран отменит себя сам, как
			// только зарегистрирует cancel.
			st.cancelRequested = true
			s.mu.Unlock()
			return nil
		}
	}
	s.mu.Unlock()
	cancel()
	return nil
}

func negotiateProtocol(params json.RawMessage) (string, error) {
	if len(params) == 0 || string(params) == "null" {
		return defaultProtocolVersion, nil
	}
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", err
	}
	if p.ProtocolVersion == "" {
		return defaultProtocolVersion, nil
	}
	for _, version := range supportedProtocolVersions {
		if p.ProtocolVersion == version {
			return version, nil
		}
	}
	return supportedProtocolVersions[0], nil
}

// handle — точка входа для тестов и внутренних вызовов: без привязки к id
// запроса, поэтому такой вызов нельзя отменить нотификацией.
func (s *Server) handle(req *Request) *Response { return s.handleCall(nil, req) }

func (s *Server) handleCall(call *inflightCall, req *Request) *Response {
	switch req.Method {
	case "initialize":
		version, err := negotiateProtocol(req.Params)
		if err != nil {
			return &Response{Error: &RPCError{Code: -32602, Message: "invalid initialize params: " + err.Error()}}
		}
		s.setClientCapabilities(req.Params)
		return &Response{Result: map[string]interface{}{
			"protocolVersion": version,
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":      map[string]interface{}{"name": "nomos", "version": buildinfo.Resolve()},
		}}
	case "ping":
		return &Response{Result: map[string]interface{}{}}
	case "tools/list":
		return &Response{Result: map[string]interface{}{"tools": toolDefs()}}
	case "tools/call":
		var p struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
			Meta      struct {
				ProgressToken json.RawMessage `json:"progressToken"`
			} `json:"_meta"`
		}
		if req.Params != nil {
			_ = json.Unmarshal(req.Params, &p)
		}
		if call != nil && len(p.Meta.ProgressToken) > 0 {
			call.progressToken = p.Meta.ProgressToken
		}
		result, isErr, rpcErr := s.callTool(call, p.Name, p.Arguments)
		if rpcErr != nil {
			return &Response{Error: rpcErr}
		}
		out := map[string]interface{}{
			"content": []map[string]interface{}{{"type": "text", "text": result}},
			"isError": isErr,
		}
		// structuredContent (2025-06-18): тот же ответ объектом, а не только
		// строкой. Клиент получает типизированные данные без разбора текста, а
		// текстовый блок остаётся для совместимости (спека этого и требует).
		if structured := structuredContentOf(result, isErr); structured != nil {
			out["structuredContent"] = structured
		}
		return &Response{Result: out}
	default:
		return &Response{Error: &RPCError{Code: -32601, Message: "unknown method: " + req.Method}}
	}
}

// issuesForJSON — замечания всегда массив.
//
// nil-срез Go сериализует как `null`, а outputSchema обещает `array`:
// structuredContent с null нарушал бы контракт, который сервер же и объявил.
// Пустой список и «нет замечаний» — одно и то же, и `[]` это показывает.
func issuesForJSON(issues []pipeline.Issue) []pipeline.Issue {
	if issues == nil {
		return []pipeline.Issue{}
	}
	return issues
}

// structuredContentOf — объектный вид результата инструмента.
//
// nil там, где объекта нет или где это ошибка исполнения: спека требует, чтобы
// structuredContent соответствовал объявленной outputSchema, а isError-результат
// описывается другой схемой (и её у инструмента нет). Молча отдать туда
// строку сообщения об ошибке значило бы нарушить контракт.
func structuredContentOf(result string, isErr bool) map[string]interface{} {
	if isErr {
		return nil
	}
	trimmed := strings.TrimSpace(result)
	if !strings.HasPrefix(trimmed, "{") {
		return nil
	}
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		return nil
	}
	return obj
}

// rawMessage — вставка уже готового JSON (progressToken приходит от клиента).
type rawMessage json.RawMessage

func (r rawMessage) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte("null"), nil
	}
	return []byte(r), nil
}

// notifyProgress — notifications/progress (2024-11-05 и новее).
//
// Спека разрешает серверу слать прогресс, только если клиент дал
// progressToken в _meta запроса: без токена клиент не сопоставит нотификацию с
// вызовом. Нет токена — молчим, а не выдумываем свой.
//
// Зачем это вообще: run_pipeline умеет ждать до 300 секунд, и клиенты с
// таймаутом на tool-call обрывали вызов раньше, чем ран доходил до гейта.
// Прогресс говорит «работа идёт» и у большинства клиентов сбрасывает таймер.
func (s *Server) notifyProgress(call *inflightCall, progress float64, total float64, message string) {
	if call == nil || call.writer == nil || len(call.progressToken) == 0 {
		return
	}
	params := map[string]interface{}{
		"progressToken": rawMessage(call.progressToken),
		"progress":      progress,
	}
	if total > 0 {
		params["total"] = total
	}
	if message != "" {
		params["message"] = message
	}
	if err := call.writer.WriteNotification("notifications/progress", params); err != nil {
		// Прогресс — не часть контракта вызова: его потеря не должна ронять
		// инструмент, но и молчать о ней нельзя.
		s.logf("notifications/progress не отправлена: %v", err)
	}
}

func rpcErr(code, msg string) *RPCError {
	if strings.HasPrefix(msg, "E_") || strings.HasPrefix(code, "E_") {
		if code == "" {
			for _, known := range []string{"E_PLUGIN_OUTSIDE_ROOT", "E_FILE_REF_OUTSIDE_ROOT", "E_FILE_REF_UNCHECKED", "E_NETWORK_DENIED", "E_PLUGIN_LOAD"} {
				if strings.HasPrefix(msg, known+":") {
					code = known
					break
				}
			}
		}
		return &RPCError{Code: -32000, Message: msg, Data: map[string]string{"code": code}}
	}
	return &RPCError{Code: -32602, Message: msg}
}

// callTool — диспетчер инструментов. call связывает вызов с id запроса, чтобы
// notifications/cancelled могла его найти; nil означает «вызов без адреса»
// (тесты, внутренние вызовы) — такой вызов отменить нотификацией нельзя.
