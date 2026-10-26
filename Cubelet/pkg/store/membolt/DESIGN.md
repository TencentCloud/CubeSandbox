# BoltCacheStore 设计文档

## 概述

BoltCacheStore 是一个泛型缓存存储实现，它将 Kubernetes 的 `cache.Store` 与 BoltDB 数据库集成，提供内存缓存和持久化存储的统一接口。

## 设计目标

1. **内存缓存性能**：利用 Kubernetes cache.Store 的高效索引和查询能力
2. **数据持久化**：使用 BoltDB 实现数据的持久化存储
3. **自动同步**：确保内存缓存和数据库始终保持一致
4. **泛型支持**：支持任意类型的对象存储，提供类型安全
5. **线程安全**：支持并发读写操作

## 架构设计

```
┌─────────────────────────────────────────┐
│      BoltCacheStore[Obj]                │
├─────────────────────────────────────────┤
│ - db: *utils.CubeStore                  │
│ - c: cache.Store                        │
│ - mu: sync.RWMutex                      │
│ - bucketName: string                    │
│ - keyFunc: cache.KeyFunc                │
├─────────────────────────────────────────┤
│ Public Methods:                         │
│ - Add(obj Obj) error                    │
│ - Update(obj Obj) error                 │
│ - Delete(obj Obj) error                 │
│ - Get(obj Obj) (interface{}, bool, err) │
│ - GetByKey(key string) (interface{}, bool, err) │
│ - List() []interface{}                  │
│ - ListKeys() []string                   │
│ - Replace(list []interface{}, rv string) error │
│ - Resync() error                        │
└─────────────────────────────────────────┘
         │                    │
         ▼                    ▼
    ┌─────────────┐    ┌──────────────┐
    │ cache.Store │    │ utils.CubeStore │
    │ (内存缓存)   │    │ (BoltDB数据库)   │
    └─────────────┘    └──────────────┘
```

## 核心组件

### 1. 字段说明

| 字段 | 类型 | 说明 |
|------|------|------|
| `db` | `*utils.CubeStore` | BoltDB 数据库实例 |
| `c` | `cache.Store` | Kubernetes 内存缓存 |
| `mu` | `sync.RWMutex` | 读写锁，保护并发访问 |
| `bucketName` | `string` | 数据库 bucket 名称，格式：`generic_<TypeName>` |
| `keyFunc` | `cache.KeyFunc` | 用于提取对象键的函数 |

### 2. 初始化流程

```go
NewBoltCacheStore[Obj](db, keyFunc, indexers)
    ↓
1. 创建 cache.Indexer 实例
2. 获取类型名称 (reflect.TypeOf)
3. 生成 bucket 名称 (generic_<TypeName>)
4. 返回 BoltCacheStore 实例
```

## 操作流程

### Add 操作

```
Add(obj)
  ↓
1. 获取写锁
2. 提取对象键 (keyFunc)
3. 序列化对象 (json.Marshal)
4. 添加到内存缓存 (c.Add)
5. 保存到数据库 (db.Set)
6. 如果数据库失败，从缓存删除并返回错误
7. 释放写锁
```

### Update 操作

```
Update(obj)
  ↓
1. 获取写锁
2. 提取对象键 (keyFunc)
3. 序列化对象 (json.Marshal)
4. 更新内存缓存 (c.Update)
5. 更新数据库 (db.Set)
6. 释放写锁
```

### Delete 操作

```
Delete(obj)
  ↓
1. 获取写锁
2. 提取对象键 (keyFunc)
3. 从内存缓存删除 (c.Delete)
4. 从数据库删除 (db.Delete)
5. 释放写锁
```

### Get 操作

```
GetByKey(key)
  ↓
1. 获取读锁
2. 从内存缓存查询 (c.GetByKey)
3. 释放读锁
4. 返回结果
```

### Replace 操作

```
Replace(list, resourceVersion)
  ↓
1. 获取写锁
2. 读取数据库中所有数据
3. 删除数据库中所有数据
4. 替换内存缓存 (c.Replace)
5. 将新列表中的所有对象保存到数据库
6. 释放写锁
```

### Resync 操作

```
Resync()
  ↓
1. 获取写锁
2. 从数据库读取所有数据 (db.ReadAll)
3. 清空内存缓存 (c.Replace)
4. 反序列化每个对象
5. 添加到内存缓存 (c.Add)
6. 释放写锁
```

## 数据流

### 写入流程

```
用户代码
  ↓
Add/Update/Delete
  ↓
┌─────────────────────────────────────┐
│ 1. 获取写锁                          │
│ 2. 提取键 (keyFunc)                 │
│ 3. 序列化 (json.Marshal)            │
│ 4. 更新缓存 (cache.Store)           │
│ 5. 更新数据库 (CubeStore)           │
│ 6. 释放写锁                          │
└─────────────────────────────────────┘
  ↓
返回 error
```

### 读取流程

```
用户代码
  ↓
Get/GetByKey/List/ListKeys
  ↓
┌─────────────────────────────────────┐
│ 1. 获取读锁                          │
│ 2. 从缓存查询 (cache.Store)         │
│ 3. 释放读锁                          │
└─────────────────────────────────────┘
  ↓
返回结果
```

## 并发安全性

### 读写锁策略

- **读操作**：使用 `RLock()`，允许多个 goroutine 并发读取
  - `Get()`
  - `GetByKey()`
  - `List()`
  - `ListKeys()`

- **写操作**：使用 `Lock()`，独占访问
  - `Add()`
  - `Update()`
  - `Delete()`
  - `Replace()`
  - `Resync()`

### 并发场景

```
场景1：并发读取
┌─────────────┐  ┌─────────────┐  ┌─────────────┐
│ GetByKey()  │  │ List()      │  │ ListKeys()  │
└──────┬──────┘  └──────┬──────┘  └──────┬──────┘
       │                │                │
       └────────────────┼────────────────┘
                        │
                   RLock (共享)
                        │
                   cache.Store
```

```
场景2：读写冲突
┌─────────────┐  ┌─────────────┐
│ GetByKey()  │  │ Update()    │
└──────┬──────┘  └──────┬──────┘
       │                │
       └────────────────┼────────────────┐
                        │                │
                   RLock vs Lock (互斥)
                        │
                   等待 Update 完成
```

## 错误处理

### 错误分类

1. **KeyFunc 错误**：提取键失败
   ```
   "failed to get key: %w"
   ```

2. **序列化错误**：JSON 序列化失败
   ```
   "failed to marshal object: %w"
   ```

3. **缓存错误**：内存缓存操作失败
   ```
   "failed to add to cache: %w"
   "failed to update cache: %w"
   "failed to delete from cache: %w"
   ```

4. **数据库错误**：数据库操作失败
   ```
   "failed to set in database: %w"
   "failed to delete from database: %w"
   "failed to read from database: %w"
   ```

### 错误恢复

- **Add 失败**：如果数据库写入失败，从缓存中删除已添加的对象
- **Update 失败**：返回错误，缓存和数据库可能不一致
- **Delete 失败**：返回错误，缓存和数据库可能不一致
- **不一致恢复**：调用 `Resync()` 从数据库恢复缓存

## 性能特性

### 时间复杂度

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

### 空间复杂度

- **内存**：O(n*m)，n = 对象数，m = 平均对象大小
- **磁盘**：O(n*m)，同上

## Bucket 命名规则

### 自动生成

```go
var obj Obj
typeName := reflect.TypeOf(obj).Name()
bucketName := fmt.Sprintf("generic_%s", typeName)
```

### 示例

| 类型 | Bucket 名称 |
|------|-----------|
| `User` | `generic_User` |
| `Pod` | `generic_Pod` |
| `Service` | `generic_Service` |
| `ConfigMap` | `generic_ConfigMap` |

### 优势

1. **自动化**：无需手动指定 bucket 名称
2. **唯一性**：每个类型有独立的 bucket
3. **可读性**：bucket 名称清晰表示存储的类型

## 扩展性

### 支持的类型

任何可以被 JSON 序列化的类型都支持：

```go
// 基本类型
store := NewBoltCacheStore[string](db, keyFunc, indexers)
store := NewBoltCacheStore[int](db, keyFunc, indexers)

// 结构体
store := NewBoltCacheStore[User](db, keyFunc, indexers)
store := NewBoltCacheStore[Pod](db, keyFunc, indexers)

// 指针
store := NewBoltCacheStore[*User](db, keyFunc, indexers)
```

### 自定义序列化

如果需要自定义序列化，可以修改 `Add` 和 `Update` 方法中的序列化逻辑。

## 最佳实践

1. **KeyFunc 设计**
   - 返回唯一的键
   - 避免返回空字符串
   - 处理错误情况

2. **错误处理**
   - 总是检查返回的错误
   - 根据错误类型采取相应的恢复措施

3. **资源管理**
   - 使用完毕后调用 `db.Close()`
   - 避免长时间持有锁

4. **并发访问**
   - 充分利用读锁的并发性能
   - 最小化写操作的持续时间

5. **数据一致性**
   - 定期调用 `Resync()` 验证一致性
   - 监控错误日志

## 测试策略

### 单元测试

- Add/Update/Delete 操作
- Get/GetByKey/List/ListKeys 查询
- Replace/Resync 操作
- 并发访问
- 错误处理

### 集成测试

- 与 CubeStore 的集成
- 与 cache.Store 的集成
- 数据持久化验证

### 性能测试

- 基准测试：Add/Update/Delete
- 大数据集测试
- 并发性能测试

## 已知限制

1. **序列化开销**：每次操作都需要 JSON 序列化/反序列化
2. **数据库 I/O**：每次写操作都会触发数据库写入
3. **内存占用**：缓存会占用内存，大数据集需要考虑内存限制
4. **类型名称**：依赖 `reflect.TypeOf().Name()`，某些情况下可能为空

## 未来改进

1. **可配置序列化**：支持自定义序列化方式（protobuf、msgpack 等）
2. **批量操作**：支持批量 Add/Update/Delete
3. **事务支持**：支持多个操作的原子性
4. **缓存淘汰**：支持 LRU 等缓存淘汰策略
5. **监控指标**：添加性能监控和指标收集
