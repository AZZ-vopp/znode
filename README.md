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
- Chỉ kết nối với ZBoard bằng nhận diện hai chiều, không chạy với V2Board gốc.

Xem thêm [tài liệu Redis và tối ưu UDP](docs/ZNODE_REDIS_UDP_VI.md).

## Cài đặt

Khuyến nghị tạo Agent/VPS trong ZBoard rồi sử dụng đúng lệnh cài đặt được sinh
trên màn hình Admin. Cài thủ công installer:

```bash
wget -N https://raw.githubusercontent.com/AZZ-vopp/znode/main/script/install.sh
bash install.sh
```

Cài agent bằng thông tin do ZBoard cấp. Token được đọc qua stdin để không xuất
hiện trong shell history hoặc danh sách process:

```bash
read -rsp 'Agent token: ' ZNODE_AGENT_TOKEN; echo
printf '%s\n' "$ZNODE_AGENT_TOKEN" | bash install.sh \
  --api-host https://panel.example.com \
  --agent-id AGENT_ID \
  --agent-token-stdin \
  --release-repo AZZ-vopp/znode \
  --release-branch main
unset ZNODE_AGENT_TOKEN
```

Installer lưu repository phát hành trong `/etc/znode/release-repo`; lệnh
`znode update` sau này tiếp tục tải đúng binary từ `AZZ-vopp/znode`.
Installer chỉ chấp nhận gói có tệp `.dgst`, kiểm tra SHA-256 trước khi giải nén
và tải script qua HTTPS có kiểm tra chứng chỉ. Bản trước được giữ tại
`/usr/local/znode.rollback` để có thể khôi phục nếu cần; không tắt kiểm tra TLS
hoặc tự thay URL tải bằng nguồn không tin cậy.

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
Mọi cấu hình hợp lệ phải có trường `"type": "zboard"` ở cấp cao nhất.

## Giấy phép

Xem [LICENSE](LICENSE). Dự án sử dụng Xray core đã tùy chỉnh theo khai báo trong
`go.mod`.

## Dữ liệu GeoIP và GeoSite

Các rule Xray dùng `geoip:` và `geosite:` cần đồng thời hai file
`geoip.dat` và `geosite.dat`. Bản phát hành tự tải dữ liệu mới nhất từ
Loyalsoldier, trình cài đặt xác thực file không rỗng rồi đặt chúng tại
`/etc/znode`. Znode tự đặt `XRAY_LOCATION_ASSET` về thư mục chứa đủ hai file;
Docker image cũng đóng gói sẵn dữ liệu vào `/etc/znode`.

Khi chạy Docker, phải gắn volume bền vững vào `/var/lib/znode`, ví dụ
`-v znode-data:/var/lib/znode`. Đây là nơi lưu batch traffic chưa được ZBoard
xác nhận; không mount thư mục này sẽ làm mất batch đang chờ khi thay container.

Có thể đổi nguồn tải khi cài bằng biến `ZNODE_GEODATA_URL`, ví dụ một mirror
nội bộ có cấu trúc `.../geoip.dat` và `.../geosite.dat`.
