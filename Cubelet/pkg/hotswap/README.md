# 配置热更新

  ## yaml 文件配置 

``` go
// 创建一个 Watcher，传入配置的类型，目前仅支持yaml 
watcher, err := NewWatcher(path, 5, &Conf{})
// 增加监听器,配置变更时通知监听器
watcher.AppendWatcher(&listenr{})
// 初始化获取文件
data, err := watcher.Init()
newConf := data.(*Conf)

```
