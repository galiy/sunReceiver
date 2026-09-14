package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// bmsDevice — одна ANT BMS из коллекции bmslistener (read_bms.php, System V
// shm 2018 на ПАК «Малина»). Формат публикации — bmslistener/bmslistener.c
// (publish_all); все поля присутствуют в ответе.
type bmsDevice struct {
	DeviceName    string    `json:"deviceName"`     // "AntBms <ёмкость> A/h"
	Port          string    `json:"port"`           // USB-порт адаптера (позиционный)
	Timestamp     int64     `json:"timestamp"`      // Unix-время последнего валидного кадра
	Time          string    `json:"time"`           // локальное время HH:MM:SS
	CellCount     int       `json:"cell_count"`     // число ячеек (S)
	CellsV        []float64 `json:"cells_v"`        // напряжения ячеек, V
	CurrentA      float64   `json:"current_a"`      // ток, A (знаковый)
	Soc           int       `json:"soc"`            // State of Charge, %
	CapacityAh    float64   `json:"capacity_ah"`    // ёмкость, А·ч
	RemainingAh   float64   `json:"remaining_ah"`   // остаточная ёмкость, А·ч
	TemperaturesC []float64 `json:"temperatures_c"` // температуры, °C (до 6 датчиков)
	ChargeMos     int       `json:"charge_mos"`     // MOS зарядки: 1 = включён
	DischargeMos  int       `json:"discharge_mos"`  // MOS разряда: 1 = включён
	Balancer      int       `json:"balancer"`       // балансировка: 1 = активна
	PowerW        float64   `json:"power_w"`        // мощность, W (знаковая)
	MaxCellIdx    int       `json:"max_cell_idx"`   // индекс ячейки с максимальным напряжением
	MaxCellV      float64   `json:"max_cell_v"`     // напряжение максимальной ячейки, V
	MinCellIdx    int       `json:"min_cell_idx"`   // индекс ячейки с минимальным напряжением
	MinCellV      float64   `json:"min_cell_v"`     // напряжение минимальной ячейки, V
	AvgCellV      float64   `json:"avg_cell_v"`     // среднее напряжение ячейки, V
	Frames        uint32    `json:"frames"`         // счётчик валидных кадров с запуска слушателя
}

// bmsCollection — ответ read_bms.php (публикация bmslistener).
type bmsCollection struct {
	Updated int64       `json:"updated"` // Unix-время публикации коллекции
	Devices []bmsDevice `json:"devices"`
}

// bmsApiClient — доступ к read_bms.php (веб-API ПАК «Малина»). Тот же хост и
// Basic-auth, что и у read_json.php (раздел "mppt" sunReceiver.json); путь
// эндпоинта задаётся полем mppt.bms_path.
type bmsApiClient struct {
	url     string
	client  *http.Client
	authHdr string
}

// bmsSite — глобальный доступ к read_bms.php; заполняется в main() из раздела
// "mppt" sunReceiver.json (поле bms_path). nil — опрос BMS отключён.
var bmsSite *bmsApiClient

// loadBmsSite собирает bmsApiClient из раздела "mppt", если в нём задано bms_path.
// Если поле отсутствует или раздел неполный — nil (BMS не опрашивается).
func loadBmsSite(sec *mpptSection) *bmsApiClient {
	if sec == nil || sec.BMSPath == "" {
		return nil
	}
	if sec.BaseURL == "" || sec.Login == "" || sec.Password == "" {
		log.Printf("bms: раздел mppt неполный (нужны base_url, login, password) — опрос BMS отключён")
		return nil
	}
	tok := base64.StdEncoding.EncodeToString([]byte(sec.Login + ":" + sec.Password))
	return &bmsApiClient{
		url:     sec.BaseURL + sec.BMSPath,
		client:  &http.Client{Timeout: 5 * time.Second},
		authHdr: "Basic " + tok,
	}
}

// fetch читает коллекцию BMS с read_bms.php.
func (s *bmsApiClient) fetch() (*bmsCollection, error) {
	req, err := http.NewRequest(http.MethodGet, s.url, nil)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	req = req.WithContext(ctx)
	req.Header.Set("Authorization", s.authHdr)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bms api get %s: %w", s.url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bms api %s: status %d", s.url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("bms api %s: read: %w", s.url, err)
	}
	var col bmsCollection
	if err := json.Unmarshal(body, &col); err != nil {
		return nil, fmt.Errorf("bms api: parse json: %w", err)
	}
	return &col, nil
}

// runBmsPoll — отдельный 1-секундный цикл опроса ANT BMS (read_bms.php на
// ПАК «Малина»; данные — C-демон bmslistener, shm 2018) и записи актуального
// состояния в отдельный Redis-ключ (HASH sunreceiver:bms). В общем снимке
// sunreceiver:current BMS не участвует (нет универсального контракта values).
func runBmsPoll(store *redisStore, stop <-chan struct{}) {
	const pollEvery = time.Second
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			pollAndSaveBMS(store)
		case <-stop:
			return
		}
	}
}

// pollAndSaveBMS делает один запрос read_bms.php и обновляет коллекцию в
// Redis: устройство появляется/исчезает с дашборда по факту наличия в ответе
// (как MPPT). При ошибке запроса предыдущее состояние в Redis сохраняется.
func pollAndSaveBMS(store *redisStore) {
	col, err := bmsSite.fetch()
	if err != nil {
		log.Printf("bms api: %v", err)
		return
	}
	active := make(map[string]string, len(col.Devices))
	for i := range col.Devices {
		d := col.Devices[i]
		if d.DeviceName == "" {
			continue
		}
		b, err := json.Marshal(d)
		if err != nil {
			log.Printf("bms: marshal %s: %v", d.DeviceName, err)
			continue
		}
		active[d.DeviceName] = string(b)
	}
	if err := store.SetBMS(active); err != nil {
		log.Printf("bms redis: %v", err)
	}
}
