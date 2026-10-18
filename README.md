# ZNode Distribution

Repository phân phối chính thức của ZNode.

Repository này chỉ chứa script cài đặt/quản lý và các binary đã biên dịch trong
[GitHub Releases](../../releases). Mã nguồn không được phân phối tại đây.

## Cài đặt agent

```bash
curl --fail --location --proto '=https' --tlsv1.2 \
  -o znode-install.sh https://raw.githubusercontent.com/AZZ-vopp/znode/main/script/install.sh
read -r -s -p 'ZNode agent token: ' ZNODE_AGENT_TOKEN; printf '\n'
printf '%s\n' "$ZNODE_AGENT_TOKEN" | bash znode-install.sh \
  --api-host 'https://your-panel.example.com' \
  --agent-id 'AGENT_ID' \
  --agent-token-stdin \
  --release-repo 'AZZ-vopp/znode' \
  --release-branch 'main'
unset ZNODE_AGENT_TOKEN
```

Hãy sử dụng lệnh riêng được tạo trong trang quản trị ZBoard để điền đúng thông
tin agent. Không chia sẻ agent token. Installer chỉ chấp nhận release có checksum
SHA-256 từ `.dgst` hoặc metadata asset của GitHub và giữ runtime hiện hành nếu
việc tải hoặc xác minh thất bại.

ZNode chỉ kết nối với ZBoard. Mọi cấu hình hợp lệ đều có `"type": "zboard"`;
binary cũ hoặc panel V2Board gốc không được chấp nhận.

## Buffer UDP của kernel

Linux cấp cho mỗi socket UDP buffer nhận mặc định `net.core.rmem_default`
(208 KiB). Một đợt tải video QUIC hoặc một khách Hysteria2 bận là đủ làm đầy
buffer đó trong vài mili giây; kernel lặng lẽ vứt gói (`UdpRcvbufErrors` tăng
trong `nstat -az`) và khách thấy phiên "mất ping" vài giây rồi tự có lại.

Runtime tự yêu cầu 4 MiB cho mọi socket UDP nó mở (listener Hysteria2/TUIC/
Shadowsocks và socket outbound freedom) qua `SO_RCVBUF`/`SO_SNDBUF`. Kernel giới
hạn yêu cầu đó ở `net.core.rmem_max`/`wmem_max`, nên installer ghi
`/etc/sysctl.d/99-znode-udp.conf` nâng trần và giá trị mặc định (chỉ nâng, không
hạ giá trị operator đã đặt cao hơn) rồi áp ngay bằng `sysctl -p`. Kiểm tra sau
khi cài hoặc cập nhật:

```bash
sysctl net.core.rmem_default net.core.rmem_max
nstat -n; sleep 60; nstat UdpRcvbufErrors   # mong đợi gần 0 drop/phút
```

Node nâng cấp từ bản cũ cần `systemctl restart znode` một lần để listener đang
mở nhận buffer mới; socket outbound tự nhận khi được tạo lại.

## Admission REALITY trước xác thực

Mỗi kết nối TCP tới inbound REALITY phải đi qua handshake trước khi được xác
thực; trong lúc đó ZNode dial đích giả trang và giữ một slot trong pool toàn
tiến trình, dùng chung cho mọi logical node trên host. Client không bật mux mở
một handshake cho mỗi kết nối TCP, nên cơn bão reconnect sau một lần mất mạng
ngắn dễ vượt pool cũ 128 slot; socket dư khi đó bị reset im lặng và người dùng
thấy "mất mạng đột ngột" dù node vẫn chạy. Từ v1.37.84 hành vi đổi như sau:

- Pool 1024 slot handshake đang chờ xác thực (trước đây 128).
- Socket đến khi pool đầy chờ tối đa 2 giây để nhận slot theo thứ tự đến, thay
  vì bị đóng ngay; hàng chờ tối đa 4096 socket, quá ngưỡng mới đóng tức thì.
- Khi vẫn phải đóng, ZNode ghi cảnh báo `REALITY pre-auth admission ...` kèm
  tổng số socket đã đóng, tối đa 10 giây một dòng. Trước đây không có log.

Kiểm tra trên node: `journalctl -u znode --since -1h | grep -c "REALITY pre-auth
admission"`; con số tăng nghĩa là node đang bị quét hoặc bão reconnect vượt cả
pool mới, cần xem `REALITY: processed invalid connection` để phân biệt scanner
với client hợp lệ. Thời gian chờ handshake 15 giây cho dial đích và ClientHello
không đổi.

## Không giới hạn số phiên (giống v2node)

ZNode không còn giới hạn số phiên mỗi tài khoản hay toàn node. Trước đây trần
mặc định 512 phiên mỗi UUID (tính cả association UDP giữ tới hết `ConnIdle`
300 giây) làm khách xem video hoặc dùng nhiều luồng QUIC bị rớt giữa chừng:
VLESS bị từ chối kết nối mới, còn Hysteria2 bị bỏ datagram không báo lỗi.

Các khoá `MaxConnectionsPerUser` và `MaxConnections` trong `ConnectionConfig`
của `/etc/znode/config.json` vẫn được đọc để file cũ không lỗi nhưng bị bỏ qua.
Bộ cài mới không ghi hai khoá này. Giới hạn thiết bị/IP của panel và
`LimitNOFILE=262144` của systemd vẫn áp dụng.

## Cách ly batch traffic bị panel từ chối vĩnh viễn

Báo cáo traffic là batch bất biến: ZNode giữ nguyên batch và report ID cho đến
khi panel ACK, nên lỗi mạng hay `5xx` không làm mất hoặc tính trùng byte. Từ
v1.37.85, khi panel trả `400`, `413` hoặc `422` (payload bị từ chối vì chính
nội dung batch, ví dụ `Invalid server traffic rate`) ba lần liên tiếp cho cùng
một report ID, ZNode ghi batch đó ra file cách ly
`<spool>.rejected-<report_id>.json` (cùng thư mục spool, quyền `0600`, kèm mã
trạng thái, thông điệp và các cặp `uid/upload/download`) rồi đẩy batch kế tiếp
lên. Hàng đợi traffic phía sau không còn bị một batch hỏng chặn vô thời hạn.
Mọi lỗi khác (`401/403`, `409`, `426`, `429`, `5xx`, lỗi mạng) đặt lại bộ đếm và
tiếp tục retry; nếu không ghi được file cách ly, batch vẫn được giữ và thử lại ở
tick sau. Log: `Traffic report rejected by the panel; immutable batch retained
for retry before quarantine` (cảnh báo, kèm `rejections=n/3`) rồi `Traffic
report was rejected permanently by the panel; immutable batch quarantined`
(kèm `path`). Operator kiểm tra `ls /var/lib/znode/traffic/*.rejected-*.json`
và xử lý nguyên nhân phía panel trước khi nhập lại nếu cần.

## Reload runtime: lý do và hoàn tất thu hồi user dở dang

Khi manifest ZBoard thay đổi, ZNode cố gắng thay chỉ inbound bị đổi trên core
Xray đang chạy. Từ v1.37.85, dòng cảnh báo `Runtime change requires full Xray
core reload` luôn kèm `reason=` nêu rõ điều kiện ép reload toàn bộ (ví dụ
`outbound "default-out" changed`, `core app "xray.app.router.Config" changed`,
`shared connection policy (ConnectionConfig) changed`, `inbound "node-12":
assignment order or tag changed`, `TLS material changed without per-node
provenance`). Ngoài ra, nếu một inbound còn user thu hồi dở dang vì lần ghi
spool trước thất bại, ZNode hoàn tất thu hồi đó (giới hạn 10 giây) trước khi
chuẩn bị reload thay vì để điều kiện này ép reload toàn bộ và rớt mọi khách
trên VPS; nếu vẫn thất bại thì log `Could not complete pending user revocation
before reload` và giữ hành vi cũ.

## Giảm bị lập chỉ mục bởi máy quét Internet

Script tùy chọn dưới đây cài một bảng nftables riêng, chỉ chặn các dải quét
Censys được công bố trên trang opt-out chính thức. Script không đổi SSH, policy
firewall hiện tại hoặc đóng cổng khách hàng:

```bash
curl --fail --location --proto '=https' --tlsv1.2 \
  -o scanner-shield.sh https://raw.githubusercontent.com/AZZ-vopp/znode/main/script/scanner-shield.sh
sh scanner-shield.sh install
```

Chạy trên panel và từng VPS ZNode. Dùng `sh scanner-shield.sh status` để kiểm tra
hoặc `sh scanner-shield.sh remove` để gỡ. Đây là opt-out theo nguồn quét đã biết,
không thể làm một IP/cổng công khai trở nên vô hình với mọi máy quét.
