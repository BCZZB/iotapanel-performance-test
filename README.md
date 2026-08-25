# 性能测试插件 (IotaPanel)

⚡ 一键完整性能测试(宝塔风格跑分)的 IotaPanel 插件。纯 Go 零依赖,单二进制常驻约 5MB,需要跑分时才执行基准。

![language](https://img.shields.io/badge/language-Go-00ADD8) ![license](https://img.shields.io/badge/license-Apache2.0-blue)

## 功能

- **CPU 基准**:整数运算、浮点运算、圆周率(π 级数迭代),多核并行,输出百万运算/秒。
- **内存基准**:大数组顺序读写带宽(MB/s),按物理内存自动降档避免 OOM。
- **磁盘基准**:顺序读/写(MB/s)、4K 随机读/写(IOPS),在插件数据目录执行,不影响系统盘根分区。
- **综合评分**:宝塔风格总分与星级。
- **历史回溯**:最近 20 次成绩留存,可对比衰减。

## 上架信息

- 仓库: https://github.com/BCZZB/iotapanel-performance-test
- 商店: https://iotapanel.plainfate.top/

## 构建

```bash
./build.sh          # 产出 bin/performance-test (amd64/arm64)
```

## 插件规范(适配 IotaPanel)

- 监听 `$PLUGIN_BIND:$PLUGIN_PORT`,经面板网关 `/p/performance-test/` 访问。
- 处理 `SIGTERM` 优雅退出,`manifest.yaml` `bind: 127.0.0.1`。
- `keepalive: true` 常驻,按需跑分,空闲低占用。

## 手动运行(调试)

```bash
PLUGIN_PORT=19200 PLUGIN_BIND=127.0.0.1 PANEL_HOME=/tmp/ptest ./bin/performance-test
# 浏览器打开 http://127.0.0.1:19200/
```

## License

Apache-2.0