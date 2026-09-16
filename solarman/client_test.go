package solarman

import (
	"context"
	"net"
	"testing"
	"time"
)

// TestExchangeCancelledOnStop — валидация механизма быстрого stop: висящий
// (медленный) логгер, который принял соединение, но не отвечает, НЕ должен
// удерживать graceful-shutdown до полного read-таймаута. При отмене ctx
// наблюдатель в Exchange закрывает сокет, и блокирующий conn.Read завершается
// немедленно. Без отмены Exchange ждал бы Timeout (30 c).
func TestExchangeCancelledOnStop(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Принять соединение и молчать (симуляция медленного/зависшего логгера).
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		time.Sleep(5 * time.Second)
	}()

	c := &Client{
		Address:    ln.Addr().String(),
		Timeout:    30 * time.Second, // без отмены ждали бы 30 с на первый байт
		IdleWindow: 30 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	frames, err := c.Exchange(ctx, BuildDeyeReadFrameFn(1, 1, 1, 0x00, 0x01, 0x03))
	elapsed := time.Since(start)

	if len(frames) != 0 {
		t.Fatalf("ожидали 0 кадров при отмене, получили %d", len(frames))
	}
	if err == nil {
		t.Fatal("ожидали ошибку при отмене ctx")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("Exchange не прервался при отмене ctx: ушло %s (должно быть < 1 с)", elapsed)
	}
	t.Logf("Exchange прерван за %s после отмены ctx", elapsed.Round(time.Millisecond))
}

// TestSolarmanMaxTotal — «drip»-логгер (отдаёт по 1 байту каждые 200 мс, интервал
// < IdleWindow, поэтому «тишина» никогда не наступает) НЕ должен растягивать
// Exchange до 4096-байтового потолка: после MaxTotal приём прекращается.
// Без MaxTotal readAll читал бы пока ctx не закроет сокет (в тесте — 5 с).
func TestSolarmanMaxTotal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Сервер: после accept шлёт по 1 байту каждые 200 мс (капает).
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			if _, err := conn.Write([]byte{0x00}); err != nil {
				return
			}
		}
	}()

	c := &Client{
		Address:    ln.Addr().String(),
		Timeout:    5 * time.Second,
		IdleWindow: 5 * time.Second,      // drip (200 мс) < IdleWindow — тишины нет
		MaxTotal:   500 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, _ = c.Exchange(ctx, BuildDeyeReadFrameFn(1, 1, 1, 0x00, 0x01, 0x03))
	elapsed := time.Since(start)

	// С MaxTotal=500 мс приём останавливается за ~500 мс; без него — ждал бы
	// до отмены ctx (5 с). Запас до 3 с, чтобы не флейковать.
	if elapsed > 3*time.Second {
		t.Fatalf("drip-соединение растянуло Exchange: ушло %s (ожидалось ~MaxTotal=500 мс)", elapsed)
	}
	t.Logf("drip: Exchange завершился за %s (MaxTotal=500 мс)", elapsed.Round(time.Millisecond))
}
