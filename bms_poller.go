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
// "map" sunReceiver.json (поле bms_path). nil — опрос BMS отключён.
var bmsSite *bmsApiClient

// loadBmsSite собирает bmsApiClient из раздела "map", если в нём задано bms_path.
// Если поле отсутствует или раздел неполный — nil (BMS не опрашивается).
func loadBmsSite(sec *mapSection) *bmsApiClient {
	if sec == nil || sec.BMSPath == "" {
		return nil
	}
	if sec.BaseURL == "" || sec.Login == "" || sec.Password == "" {
		log.Printf("bms: раздел map неполный (нужны base_url, login, password) — опрос BMS отключён")
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
func (s *bmsApiClient) fetch(ctx context.Context) (*bmsCollection, error) {
	req, err := http.NewRequest(http.MethodGet, s.url, nil)
	if err != nil {
		return nil, err
	}
	rctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req = req.WithContext(rctx)
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
//
// Параллельно 1-секундные снимки накапливаются в памяти (bmsAccumulator) в
// 5-минутные усреднённые точки: по завершении каждого промежутка точка
// пишется в Redis (месячный ZSET sunreceiver:bms:series, окно 2 календарных
// суток) и в PostgreSQL (sunreceiver.bms_averages, вечно). При остановке
// пулера накопленный (возможно неполный) промежуток дописывается.
func runBmsPoll(store *redisStore, pg *pgStore, ctx context.Context) {
	const pollEvery = time.Second
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	acc := newBmsAccumulator()
	log.Printf("bms avg: накопление 5-минутных усреднённых точек (в памяти процесса; PG %v)", pg != nil)
	for {
		select {
		case <-ticker.C:
			col := pollAndSaveBMS(ctx, store)
			if col != nil {
				now := time.Now()
				for i := range col.Devices {
					acc.add(col.Devices[i], now)
				}
			}
			saveBMSClosedBuckets(store, pg, acc.closed(time.Now()), false)
		case <-ctx.Done():
			// Drain неполного 5-минутного промежутка в Redis+PG. main() ждёт
			// завершение этой горутины (bgWg) ДО закрытия пулов Redis/PG,
			// поэтому записи не гоняются с закрытыми пулами.
			saveBMSClosedBuckets(store, pg, acc.drain(), true)
			return
		}
	}
}

// saveBMSClosedBuckets пишет готовые 5-минутные точки BMS в Redis (месячный
// ZSET, окно удержания 2 календарных суток — чистка PurgeOld) и в PG (вечно).
// partial=true — промежутки выгружаются при остановке (неполные).
func saveBMSClosedBuckets(store *redisStore, pg *pgStore, pts []bmsAvgPoint, partial bool) {
	for _, p := range pts {
		if p.avg.Samples == 0 {
			continue
		}
		sp := bmsSeriesPoint{Name: p.name, Ts: p.start.Format(time.RFC3339)}
		sp.bmsAveraged = p.avg
		if err := store.SaveBMSSeries(sp, p.start); err != nil {
			log.Printf("bms avg redis %s %s: %v", p.name, p.start.Format(time.RFC3339), err)
		}
		if pg != nil {
			if err := pg.InsertBMSAveraged(p.name, p.start, p.avg); err != nil {
				log.Printf("bms avg pg %s %s: %v", p.name, p.start.Format(time.RFC3339), err)
			}
		}
		if partial {
			log.Printf("bms avg: дописан неполный 5-минутный промежуток %s %s (снимков: %d)",
				p.name, p.start.Format(time.RFC3339), p.avg.Samples)
		}
	}
}

// pollAndSaveBMS делает один запрос read_bms.php и обновляет коллекцию в
// Redis: устройство появляется/исчезает с дашборда по факту наличия в ответе
// (как MPPT). При ошибке запроса предыдущее состояние в Redis сохраняется
// (возвращается nil).
func pollAndSaveBMS(ctx context.Context, store *redisStore) *bmsCollection {
	col, err := bmsSite.fetch(ctx)
	if err != nil {
		log.Printf("bms api: %v", err)
		return nil
	}
	// read_bms.php при сбое чтения shm отдаёт {"updated":0,"devices":[]} (HTTP 200).
	// bmslistener никогда не публикует updated=0 — это маркер сбоя: коллекцию в
	// Redis НЕ трогаем (иначе одиночная shm-гонка вычистит весь дашборд BMS).
	// Возврат nil — аккумулятор (acc.add) по nil пропустит, в него уходят только
	// валидные устройства. Валидная ПУСТАЯ коллекция (updated>0, devices=[])
	// по-прежнему чистит дашборд — этот путь ниже не тронут.
	if col.Updated == 0 {
		log.Printf("bms api: сбойный ответ (updated=0) — коллекция не трогается")
		return nil
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
	return col
}
