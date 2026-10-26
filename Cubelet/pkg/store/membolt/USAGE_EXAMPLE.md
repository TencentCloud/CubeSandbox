# BoltCacheStore 使用示例

## 概述

本文档提供了 BoltCacheStore 的详细使用示例，涵盖基本操作、高级用法和最佳实践。

## 基本使用

### 1. 定义对象类型

```go
package main

import (
    "fmt"
    "log"
    
    "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
    "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/membolt"
    "k8s.io/client-go/tools/cache"
)

// 定义用户对象
type User struct {
    ID    string `json:"id"`
    Name  string `json:"name"`
    Email string `json:"email"`
    Age   int    `json:"age"`
}
```

### 2. 初始化存储

```go
func main() {
    // 创建数据库实例
    db, err := utils.NewCubeStore("/tmp/cubelet.db", nil)
    if err != nil {
        log.Fatalf("Failed to create database: %v", err)
    }
    defer db.Close()

    // 定义 KeyFunc：用于提取对象的唯一键
    keyFunc := func(obj interface{}) (string, error) {
        user := obj.(User)
        return user.ID, nil
    }

    // 创建缓存存储
    store := membolt.NewBoltCacheStore[User](
        db,
        keyFunc,
        cache.Indexers{},
    )

    // 现在可以使用 store 进行各种操作
}
```

## 基本操作

### 添加对象

```go
// 创建用户对象
user := User{
    ID:    "user1",
    Name:  "Alice",
    Email: "alice@example.com",
    Age:   30,
}

// 添加到存储
if err := store.Add(user); err != nil {
    log.Printf("Failed to add user: %v", err)
}

// 此时：
// - 用户已添加到内存缓存
// - 用户已保存到数据库的 "generic_User" bucket 中
```

### 查询对象

```go
// 通过键查询
item, exists, err := store.GetByKey("user1")
if err != nil {
    log.Printf("Query error: %v", err)
} else if exists {
    user := item.(User)
    fmt.Printf("Found user: %s (%s)\n", user.Name, user.Email)
} else {
    fmt.Println("User not found")
}
```

### 更新对象

```go
// 修改用户信息
user.Age = 31
user.Email = "alice.new@example.com"

// 更新存储
if err := store.Update(user); err != nil {
    log.Printf("Failed to update user: %v", err)
}

// 此时：
// - 内存缓存中的用户已更新
// - 数据库中的用户也已更新
```

### 删除对象

```go
// 删除用户
if err := store.Delete(user); err != nil {
    log.Printf("Failed to delete user: %v", err)
}

// 此时：
// - 用户从内存缓存中删除
// - 用户从数据库中删除
```

## 高级操作

### 列出所有对象

```go
// 获取所有用户
users := store.List()
fmt.Printf("Total users: %d\n", len(users))

for _, item := range users {
    user := item.(User)
    fmt.Printf("  - %s: %s (age %d)\n", user.ID, user.Name, user.Age)
}
```

### 获取所有键

```go
// 获取所有用户 ID
keys := store.ListKeys()
fmt.Printf("User IDs: %v\n", keys)
```

### 批量操作

```go
// 添加多个用户
users := []User{
    {ID: "user2", Name: "Bob", Email: "bob@example.com", Age: 25},
    {ID: "user3", Name: "Charlie", Email: "charlie@example.com", Age: 35},
    {ID: "user4", Name: "Diana", Email: "diana@example.com", Age: 28},
}

for _, user := range users {
    if err := store.Add(user); err != nil {
        log.Printf("Failed to add user %s: %v", user.ID, err)
    }
}
```

### 替换所有对象

```go
// 准备新的用户列表
newUsers := []interface{}{
    User{ID: "user5", Name: "Eve", Email: "eve@example.com", Age: 32},
    User{ID: "user6", Name: "Frank", Email: "frank@example.com", Age: 29},
}

// 替换所有用户
if err := store.Replace(newUsers, ""); err != nil {
    log.Printf("Failed to replace users: %v", err)
}

// 此时：
// - 内存缓存中的所有用户被替换
// - 数据库中的所有用户也被替换
```

### 重新同步缓存

```go
// 当怀疑缓存不一致时，从数据库恢复
if err := store.Resync(); err != nil {
    log.Printf("Failed to resync: %v", err)
}

// 此时：
// - 缓存被清空
// - 所有数据从数据库重新加载到缓存
```

## 实际应用场景

### 场景 1：用户管理系统

```go
type UserManager struct {
    store *membolt.BoltCacheStore[User]
}

func NewUserManager(db *utils.CubeStore) *UserManager {
    keyFunc := func(obj interface{}) (string, error) {
        return obj.(User).ID, nil
    }
    store := membolt.NewBoltCacheStore[User](db, keyFunc, cache.Indexers{})
    return &UserManager{store: store}
}

func (um *UserManager) CreateUser(user User) error {
    return um.store.Add(user)
}

func (um *UserManager) GetUser(id string) (*User, error) {
    item, exists, err := um.store.GetByKey(id)
    if err != nil {
        return nil, err
    }
    if !exists {
        return nil, fmt.Errorf("user not found: %s", id)
    }
    user := item.(User)
    return &user, nil
}

func (um *UserManager) UpdateUser(user User) error {
    return um.store.Update(user)
}

func (um *UserManager) DeleteUser(id string) error {
    user, err := um.GetUser(id)
    if err != nil {
        return err
    }
    return um.store.Delete(*user)
}

func (um *UserManager) ListUsers() []User {
    items := um.store.List()
    users := make([]User, len(items))
    for i, item := range items {
        users[i] = item.(User)
    }
    return users
}
```

### 场景 2：缓存预热

```go
func WarmupCache(store *membolt.BoltCacheStore[User], users []User) error {
    // 清空现有缓存
    if err := store.Replace([]interface{}{}, ""); err != nil {
        return fmt.Errorf("failed to clear cache: %w", err)
    }

    // 添加新数据
    for _, user := range users {
        if err := store.Add(user); err != nil {
            return fmt.Errorf("failed to add user %s: %w", user.ID, err)
        }
    }

    return nil
}
```

### 场景 3：缓存恢复

```go
func RecoverCache(store *membolt.BoltCacheStore[User]) error {
    // 尝试从数据库恢复缓存
    if err := store.Resync(); err != nil {
        return fmt.Errorf("failed to resync cache: %w", err)
    }

    // 验证恢复结果
    items := store.List()
    fmt.Printf("Cache recovered with %d items\n", len(items))

    return nil
}
```

## 并发使用

### 并发读取

```go
import "sync"

func ConcurrentRead(store *membolt.BoltCacheStore[User]) {
    var wg sync.WaitGroup
    
    // 启动多个 goroutine 并发读取
    for i := 0; i < 10; i++ {
        wg.Add(1)
        go func(id int) {
            defer wg.Done()
            
            // 并发读取不会相互阻塞
            items := store.List()
            fmt.Printf("Goroutine %d: found %d users\n", id, len(items))
        }(i)
    }
    
    wg.Wait()
}
```

### 并发读写

```go
func ConcurrentReadWrite(store *membolt.BoltCacheStore[User]) {
    var wg sync.WaitGroup
    
    // 读取 goroutine
    for i := 0; i < 5; i++ {
        wg.Add(1)
        go func(id int) {
            defer wg.Done()
            items := store.List()
            fmt.Printf("Reader %d: found %d users\n", id, len(items))
        }(i)
    }
    
    // 写入 goroutine
    for i := 0; i < 3; i++ {
        wg.Add(1)
        go func(id int) {
            defer wg.Done()
            user := User{
                ID:   fmt.Sprintf("user_%d", id),
                Name: fmt.Sprintf("User %d", id),
            }
            if err := store.Add(user); err != nil {
                fmt.Printf("Writer %d error: %v\n", id, err)
            }
        }(i)
    }
    
    wg.Wait()
}
```

## 错误处理

### 完整的错误处理示例

```go
func SafeAdd(store *membolt.BoltCacheStore[User], user User) error {
    if err := store.Add(user); err != nil {
        // 分析错误类型
        errMsg := err.Error()
        
        switch {
        case strings.Contains(errMsg, "failed to get key"):
            return fmt.Errorf("invalid user ID: %w", err)
        case strings.Contains(errMsg, "failed to marshal"):
            return fmt.Errorf("user data serialization failed: %w", err)
        case strings.Contains(errMsg, "failed to add to cache"):
            return fmt.Errorf("cache operation failed: %w", err)
        case strings.Contains(errMsg, "failed to set in database"):
            return fmt.Errorf("database operation failed: %w", err)
        default:
            return fmt.Errorf("unknown error: %w", err)
        }
    }
    return nil
}
```

## 性能优化

### 批量操作优化

```go
func BatchAddUsers(store *membolt.BoltCacheStore[User], users []User) error {
    // 不要逐个添加，而是使用 Replace
    items := make([]interface{}, len(users))
    for i, user := range users {
        items[i] = user
    }
    
    return store.Replace(items, "")
}
```

### 缓存预热

```go
func PrewarmCache(store *membolt.BoltCacheStore[User], db *utils.CubeStore) error {
    // 从数据库读取所有数据
    allData, err := db.ReadAll("generic_User")
    if err != nil {
        return fmt.Errorf("failed to read from database: %w", err)
    }

    // 转换为对象列表
    items := make([]interface{}, 0, len(allData))
    for _, data := range allData {
        var user User
        if err := json.Unmarshal(data, &user); err != nil {
            continue
        }
        items = append(items, user)
    }

    // 一次性替换缓存
    return store.Replace(items, "")
}
```

## 完整示例程序

```go
package main

import (
    "fmt"
    "log"
    
    "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
    "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/membolt"
    "k8s.io/client-go/tools/cache"
)

type User struct {
    ID    string `json:"id"`
    Name  string `json:"name"`
    Email string `json:"email"`
    Age   int    `json:"age"`
}

func main() {
    // 初始化数据库
    db, err := utils.NewCubeStore("/tmp/cubelet.db", nil)
    if err != nil {
        log.Fatalf("Failed to create database: %v", err)
    }
    defer db.Close()

    // 创建存储
    keyFunc := func(obj interface{}) (string, error) {
        return obj.(User).ID, nil
    }
    store := membolt.NewBoltCacheStore[User](db, keyFunc, cache.Indexers{})

    // 添加用户
    fmt.Println("=== Adding users ===")
    users := []User{
        {ID: "1", Name: "Alice", Email: "alice@example.com", Age: 30},
        {ID: "2", Name: "Bob", Email: "bob@example.com", Age: 25},
        {ID: "3", Name: "Charlie", Email: "charlie@example.com", Age: 35},
    }

    for _, user := range users {
        if err := store.Add(user); err != nil {
            log.Printf("Failed to add user: %v", err)
        } else {
            fmt.Printf("Added user: %s\n", user.Name)
        }
    }

    // 列出所有用户
    fmt.Println("\n=== Listing users ===")
    for _, item := range store.List() {
        user := item.(User)
        fmt.Printf("  %s: %s (age %d)\n", user.ID, user.Name, user.Age)
    }

    // 更新用户
    fmt.Println("\n=== Updating user ===")
    alice := User{ID: "1", Name: "Alice", Email: "alice.new@example.com", Age: 31}
    if err := store.Update(alice); err != nil {
        log.Printf("Failed to update user: %v", err)
    } else {
        fmt.Println("Updated Alice's age to 31")
    }

    // 查询用户
    fmt.Println("\n=== Querying user ===")
    item, exists, _ := store.GetByKey("1")
    if exists {
        user := item.(User)
        fmt.Printf("Found: %s (age %d)\n", user.Name, user.Age)
    }

    // 删除用户
    fmt.Println("\n=== Deleting user ===")
    if err := store.Delete(users[2]); err != nil {
        log.Printf("Failed to delete user: %v", err)
    } else {
        fmt.Println("Deleted Charlie")
    }

    // 最终列表
    fmt.Println("\n=== Final user list ===")
    fmt.Printf("Total users: %d\n", len(store.List()))
    for _, item := range store.List() {
        user := item.(User)
        fmt.Printf("  %s: %s\n", user.ID, user.Name)
    }
}
```

## 测试

运行示例程序：

```bash
go run main.go
```

输出：

```
=== Adding users ===
Added user: Alice
Added user: Bob
Added user: Charlie

=== Listing users ===
  1: Alice (age 30)
  2: Bob (age 25)
  3: Charlie (age 35)

=== Updating user ===
Updated Alice's age to 31

=== Querying user ===
Found: Alice (age 31)

=== Deleting user ===
Deleted Charlie

=== Final user list ===
Total users: 2
  1: Alice
  2: Bob
```

## 总结

BoltCacheStore 提供了一个强大而灵活的缓存存储解决方案，结合了内存缓存的性能和数据库的持久化能力。通过本文档的示例，你应该能够：

1. ✅ 创建和初始化 BoltCacheStore
2. ✅ 执行基本的 CRUD 操作
3. ✅ 处理并发访问
4. ✅ 实现错误处理
5. ✅ 优化性能
6. ✅ 构建实际应用

更多信息请参考 [README.md](README.md) 和 [DESIGN.md](DESIGN.md)。
