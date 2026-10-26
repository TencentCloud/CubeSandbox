# BoltCacheStore - 泛型缓存存储

一个高性能的泛型缓存存储实现，结合了 Kubernetes `cache.Store` 的内存缓存和 BoltDB 的持久化存储。

## 特性

- 🚀 **高性能**：O(1) 读取，支持并发访问
- 💾 **持久化**：数据自动保存到 BoltDB
- 🔄 **自动同步**：缓存更新时数据库自动同步
- 🔒 **线程安全**：使用 RWMutex 保护并发访问
- 📦 **泛型支持**：支持任意可序列化的类型
- 🛡️ **容错能力**：支持从数据库恢复缓存

## 快速开始

### 安装

```bash
go get github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/membolt
```

### 基本使用

```go
package main

import (
    "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
    "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/membolt"
    "k8s.io/client-go/tools/cache"
)

type User struct {
    ID   string `json:"id"`
    Name string `json:"name"`
}

func main() {
    // 初始化数据库
    db, _ := utils.NewCubeStore("/tmp/test.db", nil)
    defer db.Close()

    // 创建存储
    store := membolt.NewBoltCacheStore[User](
        db,
        func(obj interface{}) (string, error) {
            return obj.(User).ID, nil
        },
        cache.Indexers{},
    )

    // 添加对象
    store.Add(User{ID: "1", Name: "Alice"})

    // 查询对象
    item, exists, _ := store.GetByKey("1")
    if exists {
        user := item.(User)
        println(user.Name) // 输出: Alice
    }

    // 更新对象
    store.Update(User{ID: "1", Name: "Alice Updated"})

    // 删除对象
    store.Delete(User{ID: "1", Name: "Alice Updated"})
}
```

## API 文档

### 构造函数

```go
func NewBoltCacheStore[Obj any](
    db *utils.CubeStore,
    keyFunc cache.KeyFunc,
    indexers cache.Indexers,
) *BoltCacheStore[Obj]
```

创建一个新的 BoltCacheStore 实例。

**参数：**
- `db`: BoltDB 数据库实例
- `keyFunc`: 用于提取对象键的函数
- `indexers`: 索引器配置

### 写操作

#### Add

```go
func (bcs *BoltCacheStore[Obj]) Add(obj Obj) error
```

添加对象到缓存和数据库。

#### Update

```go
func (bcs *BoltCacheStore[Obj]) Update(obj Obj) error
```

更新对象在缓存和数据库中的值。

#### Delete

```go
func (bcs *BoltCacheStore[Obj]) Delete(obj Obj) error
```

从缓存和数据库中删除对象。

### 读操作

#### Get

```go
func (bcs *BoltCacheStore[Obj]) Get(obj Obj) (item interface{}, exists bool, err error)
```

从缓存中获取对象。

#### GetByKey

```go
func (bcs *BoltCacheStore[Obj]) GetByKey(key string) (item interface{}, exists bool, err error)
```

通过键从缓存中获取对象。

#### List

```go
func (bcs *BoltCacheStore[Obj]) List() []interface{}
```

列出缓存中的所有对象。

#### ListKeys

```go
func (bcs *BoltCacheStore[Obj]) ListKeys() []string
```

列出缓存中的所有键。

### 高级操作

#### Replace

```go
func (bcs *BoltCacheStore[Obj]) Replace(list []interface{}, resourceVersion string) error
```

替换缓存和数据库中的所有对象。

#### Resync

```go
func (bcs *BoltCacheStore[Obj]) Resync() error
```

从数据库恢复缓存数据。

### 工具方法

#### GetStore

```go
func (bcs *BoltCacheStore[Obj]) GetStore() cache.Store
```

返回底层的 `cache.Store` 实例。

#### GetDatabase

```go
func (bcs *BoltCacheStore[Obj]) GetDatabase() *utils.CubeStore
```

返回底层的数据库实例。

## 数据库 Bucket 命名

BoltCacheStore 自动为每个类型创建一个 bucket，命名规则为：

```
generic_<TypeName>
```

例如：
- `User` 类型 → `generic_User`
- `Pod` 类型 → `generic_Pod`
- `Service` 类型 → `generic_Service`

## 并发安全

BoltCacheStore 使用 `sync.RWMutex` 保护所有操作：

- **读操作**（并发）：`Get`, `GetByKey`, `List`, `ListKeys`
- **写操作**（互斥）：`Add`, `Update`, `Delete`, `Replace`, `Resync`

## 错误处理

所有操作都返回 `error`，可能的错误包括：

- `"failed to get key: %w"` - KeyFunc 错误
- `"failed to marshal object: %w"` - 序列化错误
- `"failed to add to cache: %w"` - 缓存操作错误
- `"failed to set in database: %w"` - 数据库操作错误

## 性能特性

| 操作 | 时间复杂度 | 说明 |
|------|-----------|------|
| Add | O(n) | n = 序列化大小 |
| Update | O(n) | n = 序列化大小 |
| Delete | O(1) | 键查询 |
| Get | O(1) | 键查询 |
| GetByKey | O(1) | 键查询 |
| List | O(m) | m = 缓存中的对象数 |
| ListKeys | O(m) | m = 缓存中的对象数 |
| Replace | O(n*m) | n = 新对象数，m = 序列化大小 |
| Resync | O(n*m) | n = 数据库中的对象数，m = 序列化大小 |

## 测试

### 运行所有测试

```bash
go test -v ./pkg/store/membolt/
```

### 运行基准测试

```bash
go test -bench=. ./pkg/store/membolt/
```

### 测试覆盖

- ✅ Add 操作
- ✅ Update 操作
- ✅ Delete 操作
- ✅ Get/GetByKey 查询
- ✅ List/ListKeys 列表
- ✅ Replace 替换
- ✅ Resync 同步
- ✅ 并发访问
- ✅ 错误处理

## 文档

- [快速开始](#快速开始) - 5 分钟快速上手
- [使用示例](USAGE_EXAMPLE.md) - 详细的使用示例
- [设计文档](DESIGN.md) - 架构设计和实现细节

## 最佳实践

1. **KeyFunc 设计**
   ```go
   keyFunc := func(obj interface{}) (string, error) {
       user := obj.(User)
       if user.ID == "" {
           return "", fmt.Errorf("empty user ID")
       }
       return user.ID, nil
   }
   ```

2. **错误处理**
   ```go
   if err := store.Add(obj); err != nil {
       log.Printf("Failed to add object: %v", err)
       return err
   }
   ```

3. **资源管理**
   ```go
   db, err := utils.NewCubeStore(path, nil)
   if err != nil {
       return err
   }
   defer db.Close()
   ```

4. **并发访问**
   ```go
   // 充分利用读锁的并发性能
   items := store.List()
   for _, item := range items {
       // 处理 item
   }
   ```

5. **数据一致性**
   ```go
   // 定期验证一致性
   if err := store.Resync(); err != nil {
       log.Printf("Resync failed: %v", err)
   }
   ```

## 已知限制

1. **序列化开销**：每次操作都需要 JSON 序列化/反序列化
2. **数据库 I/O**：每次写操作都会触发数据库写入
3. **内存占用**：缓存会占用内存，大数据集需要考虑内存限制
4. **类型名称**：依赖 `reflect.TypeOf().Name()`，某些情况下可能为空

## 常见问题

**Q: 如何处理缓存不一致？**  
A: 调用 `store.Resync()` 从数据库恢复缓存

**Q: 支持哪些类型？**  
A: 任何可以被 JSON 序列化的类型

**Q: 是否线程安全？**  
A: 是的，使用 RWMutex 保护并发访问

**Q: 性能如何？**  
A: 读操作 O(1)，写操作 O(n)（n = 序列化大小）

**Q: 如何自定义 KeyFunc？**  
A: 在创建存储时传入自定义的 KeyFunc 函数

## 许可证

MIT

## 贡献

欢迎提交 Issue 和 Pull Request！

## 相关资源

- [Kubernetes cache.Store](https://pkg.go.dev/k8s.io/client-go/tools/cache)
- [BoltDB](https://github.com/etcd-io/bbolt)
- [CubeStore](../../utils/localstorage.go)
