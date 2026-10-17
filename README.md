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
