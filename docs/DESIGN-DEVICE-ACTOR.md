# DeviceActor：设备操作串行化

## 背景

多个后台任务可能同时需要访问同一台授权设备。直接并发执行会导致前台状态、读取结果和恢复动作互相干扰，因此设备访问需要统一的串行化入口。

## 模型

```text
caller -> queue -> DeviceActor -> device adapter
                    |
                    +-> lease / timeout / health event
```

- 一个设备同一时刻只有一个 active actor；
- 请求通过队列进入，调用方等待结果或超时返回；
- lease 到期后自动释放，避免异常任务永久占用设备；
- 所有失败转化为结构化健康事件并可触发告警；
- 手动动作和自动动作不能绕过 actor 直接操作设备。

## 适用边界

该模型适用于授权的移动设备测试、UI 自动化和设备实验室。它不提供未经授权访问第三方账户或绕过平台安全控制的方法。

实现位于 `internal/ledgermon/` 和 `internal/adbmgr/`，测试位于对应的 `*_test.go` 文件。
