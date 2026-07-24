# ZNode

ZNode là agent dành cho [AZZ-vopp/zboard](https://github.com/AZZ-vopp/zboard),
hỗ trợ chạy nhiều logical node trên một VPS và giới hạn thiết bị phân tán qua
Redis.

## Tính năng

- Một agent quản lý nhiều logical node trên cùng VPS.
- Từ chối node trùng port trước khi áp dụng cấu hình.
- Giới hạn IP/HWID/UUID thiết bị qua Redis.
- Pub/Sub đồng bộ thay đổi thiết bị gần như tức thời.
- Chia sẻ Redis connection pool giữa các logical node.
- Tối ưu bộ đếm traffic và UDP để giảm CPU/RAM.
- Gửi CPU, RAM, disk và tốc độ mạng về ZBoard.
- TLS tự ký và SHA-256 certificate pinning.
- Hỗ trợ cấu hình node thủ công cũ khi Agent bị tắt.

Xem thêm [tài liệu Redis và tối ưu UDP](docs/ZNODE_REDIS_UDP_VI.md).

## Cài đặt

Khuyến nghị tạo Agent/VPS trong ZBoard rồi sử dụng đúng lệnh cài đặt được sinh
trên màn hình Admin. Cài thủ công installer:

```bash
wget -N https://raw.githubusercontent.com/AZZ-vopp/znode/main/script/install.sh
bash install.sh
```

Cài agent bằng thông tin do ZBoard cấp:

```bash
bash install.sh \
  --api-host https://panel.example.com \
  --agent-id AGENT_ID \
  --agent-token AGENT_TOKEN \
  --release-repo AZZ-vopp/znode \
  --release-branch main
```

Installer lưu repository phát hành trong `/etc/znode/release-repo`; lệnh
`znode update` sau này tiếp tục tải đúng binary từ `AZZ-vopp/znode`.

## Biên dịch

Yêu cầu Go 1.26.1 và experiment JSON v2:

```bash
GOEXPERIMENT=jsonv2 go test ./...
GOEXPERIMENT=jsonv2 go build -v -o build_assets/znode \
  -trimpath \
  -ldflags "-X 'github.com/AZZ-vopp/znode/cmd.version=dev' -s -w -buildid="
```

## Phát hành

GitHub Actions build binary theo kiến trúc khi push mã Go lên nhánh `main` hoặc
khi tạo Release. Để installer hoạt động, repository cần có ít nhất một GitHub
Release chứa các tệp `znode-linux-<arch>.zip` do workflow tạo ra.

## Cấu hình Redis

ZBoard và ZNode phải dùng cùng Redis nếu bật đồng bộ thiết bị thời gian thực.
Giữ cùng channel đã cấu hình trên panel, mặc định `v2board:device-sync`.

Thông tin Agent được lưu tại `/etc/znode/config.json` với quyền `0600`. Không
chia sẻ Agent token hoặc sao chép file này sang VPS khác.

## Giấy phép

Xem [LICENSE](LICENSE). Dự án sử dụng Xray core đã tùy chỉnh theo khai báo trong
`go.mod`.
