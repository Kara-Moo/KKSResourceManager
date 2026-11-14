package main

// concurrency_test.go —— 并发安全测试
//
// 验证后台扫描协程运行时，前端同时读取树数据不会出现数据竞争或 panic。

import (
	"sync"
	"testing"
)



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


