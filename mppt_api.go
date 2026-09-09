package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"
)

// requestTimeout — таймаут одного HTTP-запроса к read_json.php. Ниже общего
// client.Timeout (5 с), чтобы 1-сек цикл runMapPoll не блокировался надолго.
const requestTimeout = 3 * time.Second

// mpptSite — конфигурация доступа к веб-API ПАК «Малина» для мониторинга MPPT
// (КЭС) через read_json.php?device=mppt. Источник — раздел "mppt" sunReceiver.json
// (base_url, mppt_path, login/password); пароль в открытом виде, файл в git не выгружается.
type mpptSite struct {
	BaseURL  string
	MPPTPath string
	Login    string
	Password string
	Host     string // host из base_url (IP ПАК «Малина») — используется как IP MPPT-устройства

	client  *http.Client
	authHdr string // "Basic base64(login:password)"
}

// loadMPPTSite собирает mpptSite из раздела "mppt" sunReceiver.json (mpptSection).
// Если секция отсутствует или поля не полностью заданы — возвращает nil
// (MPPT-контроллеры не опрашиваются).
func loadMPPTSite(sec *mpptSection) *mpptSite {
	if sec == nil {
		return nil
	}
	if sec.BaseURL == "" || sec.MPPTPath == "" || sec.Login == "" || sec.Password == "" {
		log.Printf("mppt site: раздел mppt неполный (нужны base_url, mppt_path, login, password) — мониторинг MPPT отключён")
		return nil
	}
	tok := base64.StdEncoding.EncodeToString([]byte(sec.Login + ":" + sec.Password))
	host := hostOf(sec.BaseURL)
	return &mpptSite{
		BaseURL:  sec.BaseURL,
		MPPTPath: sec.MPPTPath,
		Login:    sec.Login,
		Password: sec.Password,
		Host:     host,
		client:   &http.Client{Timeout: 5 * time.Second},
		authHdr:  "Basic " + tok,
	}
}

// hostOf возвращает хост/порт из базового URL (например, "192.168.13.60" из
// "http://192.168.13.60"); при ошибке парсинга — пустую строку.
func hostOf(u string) string {
	parsed, err := url.Parse(u)
	if err != nil {
		return ""
	}
	return parsed.Host
}

// mpptRaw — один элемент ответа read_json.php?device=mppt (текущие параметры
// одного MPPT-контроллера). Поля строковые (в JSON числа подаются как строки),
// timestamp — числом (Unix). Адреса RAM для справки из «Карты Памяти_MPPT».
type mpptRaw struct {
	Timestamp int64
	UID       string
	VcPV      string // Напряжение солнечных панелей, В (0x2D9)
	IcPV      string // Ток солнечных панелей, А (0x2DF)
	V_Bat     string // Напряжение АКБ, В (0x2E5)
	P_PV      string // Мощность солнечных панелей, Вт (0x2EB)
	P_Out     string // Мощность на выходе контроллера, Вт (0x2ED)
	P_Load    string // Мощность нагрузки, Вт (0x2F5)
	P_curr    string // Мощность заряда, Вт (0x2F9)
	I_Ch      string // Ток заряда АКБ, А (0x2FB)
	I_Out     string // Ток на выходе контроллера, А (0x2FF)
	Temp_Int  string // Внутренняя температура, °C (0x31D)
	Temp_Bat  string // Температура АКБ, °C (0x31E)
	Pwr_kW    string // Энергия за сутки, кВт·ч (0x3A1)
	Pwr_W     string // Энергия за сутки, Вт (0x39F)
}

// UnmarshalJSON разбирает элемент массива: числовые поля (кроме timestamp)
// приходят строками, поэтому вытаскиваем их по имени в строковые поля.
func (r *mpptRaw) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	str := func(key string, dst *string) {
		if v, ok := raw[key]; ok {
			var s string
			if json.Unmarshal(v, &s) == nil {
				*dst = s
			}
		}
	}
	str("UID", &r.UID)
	str("Vc_PV", &r.VcPV)
	str("Ic_PV", &r.IcPV)
	str("V_Bat", &r.V_Bat)
	str("P_PV", &r.P_PV)
	str("P_Out", &r.P_Out)
	str("P_Load", &r.P_Load)
	str("P_curr", &r.P_curr)
	str("I_Ch", &r.I_Ch)
	str("I_Out", &r.I_Out)
	str("Temp_Int", &r.Temp_Int)
	str("Temp_Bat", &r.Temp_Bat)
	str("Pwr_kW", &r.Pwr_kW)
	str("Pwr_W", &r.Pwr_W)
	if v, ok := raw["timestamp"]; ok {
		var n int64
		if json.Unmarshal(v, &n) == nil {
			r.Timestamp = n
			return nil
		}
		var s string
		if json.Unmarshal(v, &s) == nil {
			var n2 int64
			if _, err := fmt.Sscanf(s, "%d", &n2); err == nil {
				r.Timestamp = n2
			}
		}
	}
	return nil
}

// parseFloat читает число из строкового поля ответа API.
func parseFloat(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	var f float64
	if _, err := fmt.Sscanf(s, "%g", &f); err != nil {
		return 0, false
	}
	return f, true
}

// FetchMPPTs запрашивает текущие параметры всех MPPT-контроллеров через
// read_json.php?device=mppt и возвращает слайс контроллеров (индекс = слот).
// Per-request контекст с таймаутом requestTimeout: если ПАК «Малина» виснет,
// 1-секундный цикл runMapPoll не блокируется на общий Timeout клиента (5 с).
func (s *mpptSite) FetchMPPTs() ([]mpptRaw, error) {
	u := s.BaseURL + s.MPPTPath
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	req = req.WithContext(ctx)
	req.Header.Set("Authorization", s.authHdr)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mppt api get %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mppt api %s: status %d", u, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var arr []mpptRaw
	if err := json.Unmarshal(body, &arr); err != nil {
		return nil, fmt.Errorf("mppt api: parse json: %w", err)
	}
	return arr, nil
}

// mapMPPTAPI строит значения универсального контракта для одного MPPT-контроллера
// (слой slot) из ответа read_json.php?device=mppt:
//   - pv1_voltage/current/power = Vc_PV / Ic_PV / P_PV (панели данного контроллера);
//   - l1_voltage = V_Bat (напряжение АКБ);
//   - l1_current = I_Ch (ток заряда);
//   - ac_active_power = P_Out (мощность на выходе контроллера / заряд);
//   - energy_today = Pwr_kW + Pwr_W/1000 (общий объём выработки за сутки, кВт·ч).
//
// Возвращает также ts актуальности данных (поле timestamp ответа API).
func mapMPPTAPI(r mpptRaw) (valuesContract, time.Time, bool) {
	ts := time.Unix(r.Timestamp, 0)
	out := valuesContract{}
	var ok bool
	var v float64
	if v, ok = parseFloat(r.VcPV); ok {
		out["pv1_voltage"] = v
	}
	if v, ok = parseFloat(r.IcPV); ok {
		out["pv1_current"] = v
	}
	if v, ok = parseFloat(r.P_PV); ok {
		out["pv1_power"] = v
	}
	if v, ok = parseFloat(r.V_Bat); ok {
		out["l1_voltage"] = v
	}
	if v, ok = parseFloat(r.I_Ch); ok {
		out["l1_current"] = v
	}
	if v, ok = parseFloat(r.P_Out); ok {
		out["ac_active_power"] = v
	}
	// Общий объём выработки за сутки, кВт·ч: Pwr_kW (кВт·ч) + Pwr_W (добавочные Вт·ч).
	// Суммарно за сутки = Pwr_kW*1000 + Pwr_W (Вт·ч) → energy_today = Pwr_kW + Pwr_W/1000.
	if v, ok = parseFloat(r.Pwr_kW); ok {
		if w, ok2 := parseFloat(r.Pwr_W); ok2 {
			v += w / 1000
		}
		out["energy_today"] = v
	}
	if len(out) == 0 {
		return nil, ts, false
	}
	return out, ts, true
}
