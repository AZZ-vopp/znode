# znode

Znode hỗ trợ giới hạn IP/thiết bị phân tán qua Redis và profile giảm RAM cho
UDP. Xem [hướng dẫn Redis và UDP bằng tiếng Việt](docs/ZNODE_REDIS_UDP_VI.md).

Một VPS có thể chạy nhiều logical node bằng chế độ agent. Cài đúng một lần với
thông tin do V2Board cấp:

```bash
bash install.sh --api-host https://panel.example.com --agent-id AGENT_ID --agent-token AGENT_TOKEN
```

Nếu binary được phát hành từ fork riêng, thêm `--release-repo OWNER/REPO`.
Installer ghi nhớ repository/branch để lệnh `znode update` sau này không quay
về binary upstream. Chạy lại lệnh của cùng agent sau khi rotate token sẽ cập
nhật riêng `AgentToken`; lệnh mang Agent ID khác bị từ chối.

Installer không gửi thống kê cài đặt hoặc call-home tới dịch vụ bên thứ ba.

Agent tự lấy danh sách node được gán từ V2Board; cấu hình `Nodes` thủ công cũ
vẫn được hỗ trợ khi `Agent.Enable` là `false`. Lệnh cài agent sinh sẵn
`Agent.GlobalDeviceLimitConfig`, vì vậy giới hạn thiết bị Redis và kênh
fast-sync vẫn áp dụng cho mọi logical node. Nếu manifest có hai node trùng
`server_port`, agent từ chối cấu hình mới và giữ runtime cũ; chạy lệnh cài của
một agent khác trên VPS đã enroll cũng bị từ chối để tránh gắn nhầm máy.
Các logical node cùng cấu hình dùng chung Redis connection pool và Pub/Sub
hub, nên số kết nối nền không tăng tuyến tính theo số node.
Agent không đặt hard limit số logical node; giới hạn thực tế phụ thuộc CPU,
RAM, băng thông và số port listener còn trống trên VPS.

A v2board backend base on moddified xray-core.
Máy chủ node V2Board sử dụng phiên bản Xray core đã được tùy chỉnh.

**Lưu ý: Dự án này cần sử dụng cùng [V2Board đã được tùy chỉnh](https://github.com/wyx2685/v2board).**

## Cài đặt

### Cài đặt bằng một lệnh

```
wget -N https://raw.githubusercontent.com/wyx2685/znode/main/script/install.sh && bash install.sh
```

## Biên dịch
``` bash
GOEXPERIMENT=jsonv2 go build -v -o build_assets/znode -trimpath -ldflags "-X 'github.com/wyx2685/znode/cmd.version=$version' -s -w -buildid="
```

## Lịch sử tăng trưởng Stars

[![Stargazers over time](https://starchart.cc/wyx2685/znode.svg?variant=adaptive)](https://starchart.cc/wyx2685/znode)
