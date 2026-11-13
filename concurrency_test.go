package main

// concurrency_test.go —— 并发安全测试
//
// 验证后台扫描协程运行时，前端同时读取树数据不会出现数据竞争或 panic。

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestConcurrentScanAndTreeRead(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	// 构造一些资源文件
	for i := 0; i < 20; i++ {
		p := env.femaleCard(filepath.Join("dir", "card"+string(rune('a'+i%26))+".png"))
		writeFile(t, p, "card")
	}
	for i := 0; i < 5; i++ {
		p := filepath.Join(m.ModsDir(), "mod"+string(rune('a'+i))+".zipmod")
		makeZipmod(t, p, "guid."+string(rune('a'+i)))
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 后台：反复扫描（写依赖索引）
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				m.ScanAll(nil)
			}
		}
	}()

	// 前台：同时读取树数据
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				m.buildTree(m.SourceDir(ResourceKinds[0]), false, ResourceKinds[0])
				m.buildTree(m.DelDir(ResourceKinds[0]), true, ResourceKinds[0])
				m.buildModsTree(m.DelZipmodDir())
			}
		}
	}()

	// 触发一次实际隔离/恢复，验证写路径同样安全
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				card := env.femaleCard(filepath.Join("dir", "carda.png"))
				if _, err := os.Stat(card); err == nil {
					m.Isolate([]string{card})
					m.Restore([]string{filepath.Join(m.DelDir(ResourceKinds[0]), "dir", "carda.png")})
				}
			}
		}
	}()

	// 让并发跑一会儿
	for i := 0; i < 200; i++ {
		m.ScanAll(nil)
	}
	close(stop)
	wg.Wait()
}

func TestConcurrentTreeReadOnly(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager
	writeFile(t, env.femaleCard("a.png"), "x")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				m.buildTree(m.SourceDir(ResourceKinds[0]), false, ResourceKinds[0])
				m.buildTree(m.DelDir(ResourceKinds[0]), true, ResourceKinds[0])
			}
		}()
	}
	wg.Wait()
}

// TestConcurrentDeriveAndIndex 验证索引派生与读操作并发时无竞态。
//
// 场景：后台扫描（写索引、派生反向图）与前端读取树数据同时进行。
// 这是阶段 1 引入 index 包后最容易出问题的地方。
func TestConcurrentDeriveAndIndex(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	for i := 0; i < 30; i++ {
		writeFile(t, env.femaleCard("card"+string(rune('a'+i%26))+".png"), "x")
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 后台：反复扫描 + 派生
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				m.ScanAll(nil)
			}
		}
	}()

	// 后台：反复派生（模拟操作后重建索引）
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				m.mu.Lock()
				m.rebuildDepIndex()
				m.mu.Unlock()
			}
		}
	}()

	// 前台：读树、读依赖图
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				m.buildTree(m.SourceDir(ResourceKinds[0]), false, ResourceKinds[0])
			}
		}
	}()

	// 前台：读依赖判定
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				m.IsZipmodSafeToMove("some.guid")
			}
		}
	}()

	for i := 0; i < 100; i++ {
		m.ScanAll(nil)
	}
	close(stop)
	wg.Wait()
}
