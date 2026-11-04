// Adapted directly from the architecture and design-flow implementation in
// jfs-data.html. Keep the geometry, file cards, arrows, and flow descriptions
// in sync with that source when the storage design changes.

const BOX = {
  Avm:   { c: "vm", x: 40,  y: 56,  w: 190, h: 74,  t: "沙箱 VM", s: "guest 内存 · rootfs" },
  Acl:   { c: "cl", x: 250, y: 56,  w: 180, h: 74,  t: "Cubelet / VMM", s: "含 shim · 暂停 / 恢复 / 克隆" },
  Ajfs:  { c: "jfs", x: 250, y: 146, w: 180, h: 136, t: "JuiceFS 挂载", s: "/data/cubelet/jfs" },
  Aread: { c: "xfs", x: 32,  y: 146, w: 206, h: 64,  t: "读缓存 · xfs", s: "jfs-cache/*/raw", small: true },
  Awrite:{ c: "xfs", x: 32,  y: 218, w: 206, h: 64,  t: "写 staging · xfs", s: "jfs-cache/*/rawstaging", small: true },
  Bvm:   { c: "vm", x: 750, y: 56,  w: 190, h: 74,  t: "沙箱 VM", s: "guest 内存 · rootfs" },
  Bcl:   { c: "cl", x: 550, y: 56,  w: 180, h: 74,  t: "Cubelet / VMM", s: "含 shim · 导入 / 恢复" },
  Bjfs:  { c: "jfs", x: 550, y: 146, w: 180, h: 136, t: "JuiceFS 挂载", s: "/data/cubelet/jfs" },
  Bread: { c: "xfs", x: 742, y: 146, w: 198, h: 64,  t: "读缓存 · xfs", s: "jfs-cache/*/raw", small: true },
  Bwrite:{ c: "xfs", x: 742, y: 218, w: 198, h: 64,  t: "写 staging · xfs", s: "jfs-cache/*/rawstaging", small: true },
  redis: { c: "redis", x: 32,  y: 396, w: 190, h: 96, t: "Redis", s: "元数据 · slice / 引用计数", small: true },
  master:{ c: "master", x: 236, y: 396, w: 200, h: 96, t: "CubeMaster", s: "job · 分发 · replica", small: true },
  tc:    { c: "tc", x: 478, y: 396, w: 214, h: 96, t: "TemplateCenter", s: "拉镜像 · 生成 ext4", small: true },
  minio: { c: "minio", x: 706, y: 396, w: 226, h: 96, t: "MinIO", s: "数据块对象 ≤ 4 MiB", small: true },
};
const FRAMES = [
  { x: 20, y: 20, w: 430, h: 300, t: "A", spec: "c2 · EPYC 9754 · 16 vCPU · 30 GiB · 系统盘 500G · 数据盘 1T", c: "vm" },
  { x: 530, y: 20, w: 430, h: 300, t: "B", spec: "c3 · EPYC 9754 · 16 vCPU · 30 GiB · 系统盘 100G · 数据盘 1T", c: "cl" },
  { x: 20, y: 360, w: 940, h: 146, t: "控制节点", spec: "c1 · EPYC 9754 · 16 vCPU · 30 GiB · 系统盘 500G · 数据盘 1T", c: "master" },
];
const STATIC_LINKS = [
  ["Ajfs", "b", .3, "redis", "t", .5], ["Ajfs", "b", .75, "minio", "t", .3],
  ["Bjfs", "b", .3, "redis", "t", .75], ["Bjfs", "b", .75, "minio", "t", .5],
];

let activeBoxes = BOX;
function anchor(id, side, f = .5) {
  const b = activeBoxes[id];
  if (side === "t") return [b.x + b.w * f, b.y];
  if (side === "b") return [b.x + b.w * f, b.y + b.h];
  if (side === "l") return [b.x, b.y + b.h * f];
  return [b.x + b.w, b.y + b.h * f];
}
const NORM = { t: [0, -1], b: [0, 1], l: [-1, 0], r: [1, 0] };
function curve(a, sa, b, sb) {
  const d = Math.min(70, Math.hypot(b[0] - a[0], b[1] - a[1]) / 2.2);
  const c1 = [a[0] + NORM[sa][0] * d, a[1] + NORM[sa][1] * d];
  const c2 = [b[0] + NORM[sb][0] * d, b[1] + NORM[sb][1] * d];
  const mid = [0, 1].map((k) => (a[k] + 3 * c1[k] + 3 * c2[k] + b[k]) / 8);
  return { d: `M${a} C${c1} ${c2} ${b}`, mid, end: b, dir: NORM[sb] };
}
const esc = (s) => String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;");
function label(x, y, text) {
  const w = [...text].reduce((n, ch) => n + (ch.charCodeAt(0) > 255 ? 12.5 : 7.2), 0) + 16;
  return `<g class="lbl"><rect x="${x - w / 2}" y="${y - 11}" width="${w}" height="22" rx="5"/><text x="${x}" y="${y + 4.5}" text-anchor="middle">${esc(text)}</text></g>`;
}
function rectsOverlap(a, b, pad = 0) {
  return a.x < b.x + b.width + pad && a.x + a.width > b.x - pad &&
    a.y < b.y + b.height + pad && a.y + a.height > b.y - pad;
}
function layoutArrowLabels(svg) {
  const obstacles = [...svg.querySelectorAll(".box.on text, .t-frame")].map((node) => node.getBBox());
  const occupied = [];
  const raisedLabels = [];
  const offsets = [[0, 0]];
  for (const r of [26, 44, 64, 88, 116, 148, 180]) {
    offsets.push([0, -r], [r, 0], [0, r], [-r, 0],
      [r, -r], [r, r], [-r, r], [-r, -r],
      [r / 2, -r], [r, -r / 2], [r, r / 2], [r / 2, r],
      [-r / 2, r], [-r, r / 2], [-r, -r / 2], [-r / 2, -r]);
  }
  for (const lbl of svg.querySelectorAll(".lbl")) {
    const base = lbl.getBBox();
    let chosen = [0, 0];
    for (const [dx, dy] of offsets) {
      const candidate = { x: base.x + dx, y: base.y + dy, width: base.width, height: base.height };
      const inBounds = candidate.x >= 20 && candidate.x + candidate.width <= 960 &&
        candidate.y >= 18 && candidate.y + candidate.height <= 500;
      if (!inBounds || obstacles.some((r) => rectsOverlap(candidate, r, 5)) ||
          occupied.some((r) => rectsOverlap(candidate, r, 6))) continue;
      chosen = [dx, dy];
      break;
    }
    const placed = { x: base.x + chosen[0], y: base.y + chosen[1], width: base.width, height: base.height };
    occupied.push(placed);
    if (chosen[0] || chosen[1]) {
      lbl.setAttribute("transform", `translate(${chosen[0]} ${chosen[1]})`);
      const leader = document.createElementNS("http://www.w3.org/2000/svg", "path");
      const x1 = base.x + base.width / 2, y1 = base.y + base.height / 2;
      const x2 = placed.x + placed.width / 2, y2 = placed.y + placed.height / 2;
      leader.setAttribute("class", "label-leader");
      leader.setAttribute("d", `M${x1},${y1} L${x2},${y2}`);
      lbl.parentNode.insertBefore(leader, lbl);
    }
    const arrowGroup = lbl.parentNode, layer = arrowGroup.parentNode;
    const color = arrowGroup.style.getPropertyValue("--c");
    const leader = lbl.previousElementSibling?.classList.contains("label-leader") ? lbl.previousElementSibling : null;
    if (leader) {
      leader.style.setProperty("--c", color);
      layer.appendChild(leader);
    }
    lbl.style.setProperty("--c", color);
    raisedLabels.push([layer, lbl]);
  }
  // Keep every label above every animated arrow; otherwise a later arrow can cut
  // through a label emitted by an earlier arrow.
  for (const [layer, lbl] of raisedLabels) layer.appendChild(lbl);
}
function badgeSVG(b) {
  const tw = (t, big) => [...t].reduce((n, ch) => n + (ch.charCodeAt(0) > 255 ? (big ? 15 : 12) : (big ? 8.6 : 6.8)), 0);
  const head = `${b.no}  ${b.k}`, w1 = tw(head, true) + 28, w2 = tw(b.time, true) + 28;
  const w = w1 + w2, x0 = 490 - w / 2;
  let s = `<g><rect x="${x0}" y="6" width="${w}" height="34" rx="9" fill="var(--card)" stroke="var(--fg)" stroke-width="1.5"/>` +
    `<rect x="${x0 + w1}" y="6" width="${w2}" height="34" rx="9" fill="var(--fg)"/><rect x="${x0 + w1}" y="6" width="12" height="34" fill="var(--fg)"/>` +
    `<text x="${x0 + 14}" y="29" style="font-size:15px;font-weight:600">${esc(head)}</text>` +
    `<text x="${x0 + w1 + 14}" y="29" style="font-size:15px;font-weight:700;fill:var(--bg)">${esc(b.time)}</text>`;
  if (b.sub) s += `<text class="t-sub" x="490" y="58" text-anchor="middle" style="font-size:12px">${esc(b.sub)}</text>`;
  return s + `</g>`;
}
const JFS_IDS = new Set(["Ajfs", "Aread", "Awrite", "Bjfs", "Bread", "Bwrite", "redis", "minio"]);
function noteWidth(text) {
  return [...text].reduce((n, ch) => n + (ch.charCodeAt(0) > 255 ? 13 : 7.4), 0) + 22;
}
function summaryCallouts() {
  const jfs = "var(--c-jfs)", redisC = "var(--c-redis)", cache = "var(--c-xfs)";
  const notes = [
    { side: "l", y: 201, text: "文件视图", c: jfs, d: "M6,214 L262,214 L262,258 M262,214 L266,214 M262,258 L266,258", dots: [[266, 214], [266, 258]] },
    { side: "l", y: 360, text: "元数据和数据分离", c: redisC, d: "M6,373 L6,501 M6,444 L32,444", dots: [[32, 444]] },
    { side: "l", y: 392, text: "快速复制", c: redisC, d: "", dots: [] },
    { side: "l", y: 424, text: "元数据压缩", c: redisC, d: "", dots: [] },
    { side: "l", y: 456, text: "备份", c: redisC, d: "", dots: [] },
    { side: "l", y: 488, text: "恢复", c: redisC, d: "", dots: [] },
    { side: "r", y: 46, text: "FUSE 开发易修改", c: jfs, d: "M978,59 L978,141 L430,141 M730,141 L730,146 M430,141 L430,146", dots: [[430, 146], [730, 146]] },
    { side: "r", y: 78, text: "支持平滑升级", c: jfs, d: "", dots: [] },
    { side: "r", y: 165, text: "本地 cache", c: cache, d: "M978,178 L940,178", dots: [[940, 178]] },
    { side: "r", y: 237, text: "丰富的缓存策略", c: cache, d: "M978,250 L940,250", dots: [[940, 250]] },
  ];
  return notes.map((n) => {
    const w = noteWidth(n.text), x = n.side === "l" ? 6 - w : 978;
    const dots = n.dots.map(([dx, dy]) => `<circle class="sumdot" cx="${dx}" cy="${dy}" r="2.6"/>`).join("");
    return `<g class="sumnote" style="--c:${n.c}"><path class="sumlink" d="${n.d}"/>${dots}<rect x="${x}" y="${n.y}" width="${w}" height="26" rx="6"/><text x="${x + w / 2}" y="${n.y + 17}" text-anchor="middle">${esc(n.text)}</text></g>`;
  }).join("");
}
function archSVG(on, arrows, badge, opt = {}) {
  const all = on == null, top = badge ? 62 : 0;
  const sum = !!opt.summary;
  const sumJfs = sum && !opt.xfs;
  const padL = sum ? 160 : 0, padR = sum ? 176 : 0;
  const sumH = sum ? 536 : badge ? 512 + top : 500;
  let s = `<svg viewBox="${12 - padL} ${badge ? 0 : 12} ${956 + padL + padR} ${sumH}" preserveAspectRatio="xMidYMid meet" xmlns="http://www.w3.org/2000/svg">` +
    (badge ? badgeSVG(badge) : "") + `<g transform="translate(0,${top})">`;
  const boxes = { ...BOX };
  const hide = new Set();
  if (opt.xfs) {
    boxes.Ajfs = { c: "xfs", x: 32, y: 146, w: 398, h: 136, t: "XFS 存储", s: "/data/cubelet/storage/xfs" };
    boxes.Bjfs = { c: "xfs", x: 550, y: 146, w: 390, h: 136, t: "XFS 存储", s: "/data/cubelet/storage/xfs" };
    ["Aread", "Awrite", "Bread", "Bwrite"].forEach((id) => hide.add(id));
  }
  if (sumJfs) {
    const img = (node, i, id, t) => ({ id, t, s: "", c: "jfs", file: true, x: node === "B" ? 566 : 266, y: i === 0 ? 196 : 240, w: 148, h: 36, small: true });
    for (const f of [img("A", 0, "sa-root", "rootfs 镜像"), img("A", 1, "sa-mem", "内存镜像"), img("B", 0, "sb-root", "rootfs 镜像"), img("B", 1, "sb-mem", "内存镜像")]) boxes[f.id] = f;
  }
  for (const f of opt.files || []) boxes[f.id] = f;
  const saved = activeBoxes;
  activeBoxes = boxes;
  for (const f of FRAMES) s += `<g style="--c:${opt.jfsOnly ? "var(--s3)" : `var(--c-${f.c})`}"><rect class="frame" x="${f.x}" y="${f.y}" width="${f.w}" height="${f.h}" rx="12"/><text class="t-frame" x="${f.x + 14}" y="${f.y + 22}">${f.t}</text>` +
    (opt.jfsOnly && !opt.summary && f.spec ? `<text class="t-sub" style="font-size:10.5px" x="${f.x + 14 + [...f.t].reduce((n, ch) => n + (ch.charCodeAt(0) > 255 ? 13 : 7.4), 16)}" y="${f.y + 21}">${esc(f.spec)}</text>` : "") + `</g>`;
  if (!opt.xfs) {
    for (const l of STATIC_LINKS) { const c = curve(anchor(l[0], l[1], l[2]), l[1], anchor(l[3], l[4], l[5]), l[4]); s += `<path class="link" style="--c:var(--c-${boxes[l[3]].c})" d="${c.d}"/>`; }
    s += `<path class="link" style="--c:var(--c-jfs);stroke-width:2" d="M430,214 L550,214"/>` + `<text class="t-sub" x="490" y="206" text-anchor="middle">同一个卷</text><text class="t-sub" x="490" y="232" text-anchor="middle">同一路径</text>`;
  }
  for (const [id, b] of Object.entries(boxes)) {
    if (hide.has(id)) continue;
    const cls = (all ? "" : on.includes(id) ? " on" : " dim") + (b.clone ? " clone" : "");
    const files = opt.files || [];
    const summaryMount = sumJfs && (id === "Ajfs" || id === "Bjfs");
    const head = !summaryMount && ((id === "Ajfs" && files.some((f) => f.x < 500)) || (id === "Bjfs" && files.some((f) => f.x >= 500)));
    const ty = summaryMount ? b.y + 18 : !b.s ? b.y + b.h / 2 + 4 : b.top ? b.y + 26 : b.y + b.h / 2 - (b.small ? 1 : 3);
    const bc = opt.jfsOnly && !JFS_IDS.has(id) && !b.file ? "var(--s3)" : `var(--c-${b.c})`;
    s += `<g class="box${cls}" style="--c:${bc}"><rect class="bg" x="${b.x}" y="${b.y}" width="${b.w}" height="${b.h}" rx="8"/>` +
      `<rect class="bar" x="${b.x + 6}" y="${b.y + 10}" width="4" height="${Math.min(b.h - 20, 54)}" rx="2"/>` +
      (head ? "" : `<text class="t-title" x="${b.x + b.w / 2 + 6}" y="${ty}" text-anchor="middle" ${b.small ? 'style="font-size:13px"' : ""}>${b.t}</text>` +
      (b.s ? `<text class="t-sub" x="${b.x + b.w / 2 + 6}" y="${ty + 18}" text-anchor="middle" ${b.small ? 'style="font-size:10.5px"' : ""}>${b.s}</text>` : "")) + `</g>`;
  }
  for (const a of arrows || []) {
    const c = curve(anchor(a[0], a[1], a[2]), a[1], anchor(a[3], a[4], a[5]), a[4]);
    const [ex, ey] = c.end, [dx, dy] = c.dir;
    const px = -dy, py = dx;
    s += `<g style="--c:var(--c-${boxes[a[3]].c})"><path class="arrow" d="${c.d}"/>` +
      `<path class="head" d="M${ex},${ey} L${ex + dx * 9 + px * 5},${ey + dy * 9 + py * 5} L${ex + dx * 9 - px * 5},${ey + dy * 9 - py * 5} Z"/>`;
    if (a[6]) s += Array.isArray(a[7]) ? label(a[7][0], a[7][1], a[6]) : label(c.mid[0] + (a[7] || 0), c.mid[1] + (a[8] || 0), a[6]);
    s += `</g>`;
  }
  if (sumJfs) s += summaryCallouts();
  activeBoxes = saved;
  return s + `</g></svg>`;
}


const M2A = [430, 340], A2M = [500, 340];
function xfsFile(i, n, id, t, s, clone) {
  const w = n === 1 ? 360 : 176;
  return { id, t, s, c: "xfs", clone: !!clone, x: n === 1 ? 50 : 48 + i * 192, y: 206, w, h: 60, small: true };
}
function jfsFileCol(i, id, t, s, node, clone) {
  return { id, t, s, c: "jfs", clone: !!clone, x: node === "B" ? 560 : 260, y: 154 + i * 64, w: 160, h: 56, small: true };
}
const cubefsTplWrite = {
  k: "写两个文件", time: "/data/cubelet/jfs", on: ["Acl", "Avm", "Ajfs", "Awrite", "redis", "f-root", "f-mem"],
  files: [jfsFileCol(0, "f-root", "模板 rootfs", "tpl-*-rootfs"), jfsFileCol(1, "f-mem", "模板内存", "tpl-*-memory")],
  arrows: [["Acl", "b", .5, "f-root", "t", .5, "写 rootfs", 46, -6], ["Avm", "b", .93, "f-mem", "l", .1, "写内存", -36, -28], ["Ajfs", "l", .84, "Awrite", "r", .78, "staging", 0, 26], ["Ajfs", "b", .42, "redis", "t", .42, "元数据"]],
  d: [
    "A 拉起临时沙箱做 AppSnapshot，在挂载点 /data/cubelet/jfs 上创建模板 rootfs 和模板内存。文件在该节点的 cube/nodes/<node> 目录下。",
    "创建空文件只在 Redis 增加 inode。写入数据时，新块进入本机 staging jfs-cache/*/rawstaging，对应 slice 同时写入 Redis，这次写即可返回。",
    "JuiceFS 在后台把 staging 里的块上传到对象存储。上传完成前，其他节点已经能看到这两个文件，还不能安全读取其中的块。",
  ],
};
const FLOWS = {
  xfs: [
    { steps: [
      { k: "交给 TemplateCenter", time: "create-from-image", on: ["master", "tc"],
        arrows: [["master", "t", .8, "tc", "t", .2, "提交构建", [455, 346]]],
        d: ["制作模板从 CubeMaster 开始。create-from-image 在 Master 落一条 job。", "Master 不自己拉镜像，把构建转给控制节点上的 TemplateCenter。"] },
      { k: "生成 ext4", time: "TemplateCenter", on: ["tc", "master"],
        arrows: [["tc", "t", .25, "master", "t", .7, "回报 ext4", [455, 346]]],
        d: ["TemplateCenter 拉取 OCI 镜像，解包做成 ext4 rootfs artifact，再回调 CubeMaster。", "这时计算节点上还没有模板文件。"] },
      { k: "分发到每个节点", time: "xfs 各建一份", on: ["master", "Acl"],
        arrows: [["master", "t", .35, "Acl", "r", .5, "CreateImage", M2A]],
        d: ["xfs 模板不是共享的一份。distribution_scope 里每个节点都要各建一次。", "Master 先让 A 的 Cubelet 把 ext4 artifact 拉到本机。B 随后再做一遍。"] },
      { k: "写两个文件", time: "/data/cubelet/storage/xfs", on: ["Acl", "Avm", "Ajfs", "f-root", "f-mem"],
        files: [xfsFile(0, 2, "f-root", "模板 rootfs", "tpl-*-rootfs"), xfsFile(1, 2, "f-mem", "模板内存", "tpl-*-memory")],
        arrows: [["Acl", "b", .55, "f-root", "t", .5, "写 rootfs"], ["Avm", "b", .45, "f-mem", "t", .5, "写内存"]],
        d: ["Cubelet 用这份 ext4 拉起临时沙箱，AppSnapshot 在 /data/cubelet/storage/xfs 里写下两个文件。", "一个是模板 rootfs，一个是整份内存底稿。后面都按文件来用，不再对着整块 XFS。"] },
      { k: "本节点就绪", time: "replica", on: ["Acl", "master"],
        arrows: [["Acl", "r", .75, "master", "t", .4, "READY", A2M]],
        d: ["临时沙箱销毁。Cubelet 写入本地 catalog，CubeMaster 把 A 的 replica 标成 READY。", "B 不能用 A 的文件，轮到 B 时再下载 ext4，再写一份 rootfs 和内存镜像。"] },
    ] },
    { steps: [
      { k: "reflink rootfs", time: "只复制 rootfs", on: ["Acl", "Ajfs", "f-src", "f-sb"],
        files: [xfsFile(0, 2, "f-src", "模板 rootfs", "tpl-*-rootfs"), xfsFile(1, 2, "f-sb", "沙箱 rootfs", "sb-*-rootfs", true)],
        arrows: [["f-src", "r", .5, "f-sb", "l", .5, "reflink", 0, -32]],
        d: ["Cubelet 用 reflink 从模板 rootfs 文件拷出沙箱自己的 rootfs 文件。", "内存文件这一步不动。新 rootfs 和模板共享底层数据块。"] },
      { k: "映射内存", time: "MAP_PRIVATE", on: ["Avm", "Ajfs", "f-mem"],
        files: [xfsFile(0, 1, "f-mem", "模板内存", "tpl-*-memory")],
        arrows: [["f-mem", "t", .35, "Avm", "b", .45, "MAP_PRIVATE"]],
        d: ["启动不 reflink 内存。VMM 直接 MAP_PRIVATE 映射模板内存这一个文件。", "guest 写过的页变成匿名脏页，不改模板文件。写盘写上一步那个沙箱 rootfs。"] },
    ] },
    { steps: [
      { k: "运行中", time: "持续", on: ["Avm", "Ajfs", "f-mem", "f-sb"],
        files: [xfsFile(0, 2, "f-mem", "模板内存", "正在映射"), xfsFile(1, 2, "f-sb", "沙箱 rootfs", "sb-*-rootfs", true)],
        arrows: [["f-mem", "t", .4, "Avm", "b", .35, "MAP_PRIVATE"], ["Avm", "b", .7, "f-sb", "t", .5, "写盘"]],
        d: ["内存仍映射模板内存文件，差分只在 VM 的匿名脏页里。", "磁盘差分写进沙箱 rootfs 文件，覆盖处由 XFS 做 CoW。"] },
      { k: "磁盘", time: "排空再 reflink", on: ["Avm", "Acl", "Ajfs", "f-live", "f-root"],
        files: [xfsFile(0, 2, "f-live", "沙箱 rootfs", "sb-*-rootfs", true), xfsFile(1, 2, "f-root", "暂停 rootfs", "tpl-pause-rootfs", true)],
        arrows: [["Avm", "b", .25, "f-live", "t", .4, "排空 virtio-blk"], ["f-live", "r", .5, "f-root", "l", .5, "reflink", 0, -14]],
        d: ["先排空 virtio-blk 里还在飞的请求，这些写落到正在用的沙箱 rootfs。", "再从这份沙箱 rootfs reflink 出暂停 rootfs。"] },
      { k: "内存", time: "reflink 再写脏页", on: ["Avm", "Acl", "Ajfs", "f-base", "f-mem"],
        files: [xfsFile(0, 2, "f-base", "内存底稿", "正在映射"), xfsFile(1, 2, "f-mem", "暂停内存", "tpl-pause-memory", true)],
        arrows: [["f-base", "r", .5, "f-mem", "l", .5, "reflink", 0, -14], ["Avm", "b", .7, "f-mem", "t", .45, "写回脏页"]],
        d: ["先从正在映射的内存底稿 reflink 出暂停内存。", "再扫描页表，拿到这次的脏页。", "然后把脏页写回这份暂停内存。"] },
      { k: "本机恢复", time: "映射内存", on: ["Avm", "Acl", "Ajfs", "f-mem", "f-sb"],
        files: [xfsFile(0, 2, "f-mem", "暂停内存", "直接映射", true), xfsFile(1, 2, "f-sb", "沙箱 rootfs", "再 reflink", true)],
        arrows: [["f-mem", "t", .4, "Avm", "b", .4, "MAP_PRIVATE"], ["Acl", "b", .7, "f-sb", "t", .5, "reflink rootfs"]],
        d: ["内存不再 reflink。VMM 直接 MAP_PRIVATE 映射暂停包里的内存文件。", "rootfs 会从暂停包再 reflink 成沙箱自己的文件，因为恢复后还要写盘。"] },
    ] },
  ],
  "cubefs-local": [
    { steps: [
      { k: "交给 TemplateCenter", time: "create-from-image", on: ["master", "tc"],
        arrows: [["master", "t", .8, "tc", "t", .2, "提交构建", [455, 346]]],
        d: ["制作模板从 CubeMaster 开始。create-from-image 在 Master 落一条 job。", "Master 不自己拉镜像，把构建转给控制节点上的 TemplateCenter。"] },
      { k: "生成 ext4", time: "TemplateCenter", on: ["tc", "master"],
        arrows: [["tc", "t", .25, "master", "t", .7, "回报 ext4", [455, 346]]],
        d: ["TemplateCenter 拉取 OCI 镜像，解包做成 ext4 rootfs artifact，再回调 CubeMaster。", "这时计算节点上还没有模板文件。"] },
      { k: "只挑一个节点", time: "只建一次", on: ["master", "Acl"],
        arrows: [["master", "t", .3, "Acl", "r", .5, "CreateImage", M2A]],
        d: ["cubefs 模板在共享卷上只建一次。Master 在 distribution_scope 里随机挑一个可调度节点，这里是 A。", "B 不重复构建。"] },
      cubefsTplWrite,
      { k: "上传对象存储", time: "后台", on: ["Ajfs", "Awrite", "minio"],
        arrows: [["Awrite", "b", .5, "minio", "t", .25, "PUT"]],
        d: ["writeback 把 staging 里的 rootfs 和内存块上传到对象存储。", "上传完成前，别的节点虽然能看到元数据，还不能安全读取这些块。"] },
      { k: "标记 READY", time: "一条记录", on: ["Acl", "master"],
        arrows: [["Acl", "r", .7, "master", "t", .45, "READY", A2M]],
        d: ["CubeMaster 只记一条副本记录，用来提供构建时的组件版本。", "从 cubefs 模板创建沙箱时不检查每个节点有没有本地副本。"] },
    ] },
    { steps: [
      { k: "克隆 rootfs", time: "copy_file_range", on: ["Acl", "Ajfs", "redis", "f-src", "f-sb"],
        files: [jfsFileCol(0, "f-src", "模板 rootfs", "tpl-*-rootfs"), jfsFileCol(1, "f-sb", "沙箱 rootfs", "sb-*-rootfs", "", true)],
        arrows: [["Acl", "b", .08, "f-sb", "l", .25, "copy_file_range", -20, 6], ["Ajfs", "b", .55, "redis", "t", .45, "元数据"]],
        d: ["在 JuiceFS 内用 copy_file_range 从模板 rootfs 文件克隆出沙箱 rootfs。", "只复制 slice 引用。内存文件这一步不复制。"] },
      { k: "映射内存", time: "MAP_PRIVATE", on: ["Avm", "Ajfs", "f-mem"],
        files: [jfsFileCol(0, "f-mem", "模板内存", "tpl-*-memory")],
        arrows: [["f-mem", "l", .2, "Avm", "r", .85, "MAP_PRIVATE"]],
        d: ["本机启动不复制内存。VMM 直接 MAP_PRIVATE 映射模板内存文件。", "guest 写页变成匿名脏页。写盘写上一步那个沙箱 rootfs，在 JuiceFS 上产生新的 slice。"] },
      { k: "读进缓存", time: "本地 XFS", on: ["Ajfs", "Aread", "minio"],
        arrows: [["minio", "t", .3, "Aread", "b", .5, "GET"], ["Aread", "r", .5, "Ajfs", "l", .4, "raw"]],
        d: ["第一次缺页从对象存储把块拉进读缓存 jfs-cache/*/raw。", "之后本机再读同一块，走这份 XFS 缓存。"] },
    ] },
    { steps: [
      { k: "运行中", time: "持续", on: ["Avm", "Ajfs", "Aread", "f-root", "f-mem"],
        files: [jfsFileCol(0, "f-root", "沙箱 rootfs", "sb-*-rootfs", "", true), jfsFileCol(1, "f-mem", "模板内存", "正在映射")],
        arrows: [["Avm", "r", .9, "f-root", "l", .2, "写盘"], ["f-mem", "l", .1, "Avm", "b", .93, "MAP_PRIVATE", -24, -16]],
        d: ["内存差分：MAP_PRIVATE 映射内存镜像，guest 写页只留在 VM 里。", "磁盘差分：rootfs 是模板的 copy_file_range 克隆，guest 写盘直接写这个文件。读过的块在读缓存里。"] },
      { k: "磁盘", time: "排空再克隆", on: ["Avm", "Acl", "Ajfs", "Awrite", "redis", "f-live", "f-root"],
        files: [jfsFileCol(0, "f-live", "沙箱 rootfs", "sb-*-rootfs", "", true), jfsFileCol(1, "f-root", "暂停 rootfs", "暂停包", "", true)],
        arrows: [["Avm", "r", .9, "f-live", "l", .2, "排空 virtio-blk"], ["Ajfs", "l", .42, "Awrite", "r", .55, "staging"], ["Acl", "b", .08, "f-root", "l", .25, "copy_file_range", -20, 6], ["Ajfs", "b", .55, "redis", "t", .45, "元数据"]],
        d: ["先排空 virtio-blk 里还在飞的请求，这些写落到正在用的沙箱 rootfs，并进入写 staging。", "再从这份沙箱 rootfs copy_file_range 出暂停 rootfs，只复制 slice 引用。"] },
      { k: "内存", time: "克隆再写脏页", on: ["Avm", "Acl", "Ajfs", "Awrite", "redis", "f-base", "f-mem"],
        files: [jfsFileCol(0, "f-base", "内存底稿", "正在映射"), jfsFileCol(1, "f-mem", "暂停内存", "暂停包", "", true)],
        arrows: [["Acl", "b", .5, "f-base", "t", .5, "copy_file_range"], ["Avm", "b", .93, "f-mem", "l", .25, "写回脏页", -28, 8], ["Ajfs", "l", .78, "Awrite", "r", .4, "staging"]],
        d: ["先 copy_file_range 内存底稿，只复制引用，这时还没有这次的脏页。", "再扫描页表拿到脏页。", "脏页按 1 MiB 窗口写回暂停内存，进入写 staging。元数据提交后暂停返回。"] },
      { k: "上传", time: "后台", on: ["Ajfs", "Awrite", "minio"],
        arrows: [["Awrite", "b", .55, "minio", "t", .28, "PUT"]],
        d: ["writeback 把 rawstaging 里的块上传到对象存储，成功后删掉对应 staging 文件。", "本机恢复不用等这次上传。"] },
      { k: "本机恢复", time: "映射内存", on: ["Avm", "Acl", "Ajfs", "Aread", "Awrite", "f-mem", "f-root"],
        files: [jfsFileCol(0, "f-root", "沙箱 rootfs", "再克隆", "", true), jfsFileCol(1, "f-mem", "暂停内存", "直接映射", "", true)],
        arrows: [["Acl", "b", .5, "f-root", "t", .5, "copy_file_range"], ["f-mem", "l", .1, "Avm", "b", .93, "MAP_PRIVATE", -24, -16], ["Awrite", "r", .45, "f-mem", "l", .55, "staging"]],
        d: ["内存不再 copy_file_range。VMM 直接 MAP_PRIVATE 映射暂停包里的内存文件。", "rootfs 再克隆成沙箱自己的文件。刚写过的脏页和磁盘块还在写 staging，本机恢复可以直接读；已经进读缓存的块走读缓存。"] },
    ] },
  ],
  "cubefs-cross": [
    { steps: [
      { k: "交给 TemplateCenter", time: "create-from-image", on: ["master", "tc"],
        arrows: [["master", "t", .8, "tc", "t", .2, "提交构建", [455, 346]]],
        d: ["制作模板从 CubeMaster 开始。create-from-image 在 Master 落一条 job。", "Master 把构建转给控制节点上的 TemplateCenter。"] },
      { k: "生成 ext4", time: "TemplateCenter", on: ["tc", "master"],
        arrows: [["tc", "t", .25, "master", "t", .7, "回报 ext4", [455, 346]]],
        d: ["TemplateCenter 拉取 OCI 镜像，解包做成 ext4 rootfs artifact，再回调 CubeMaster。", "计算节点上这时还没有模板文件。"] },
      { k: "只挑一个节点", time: "只建一次", on: ["master", "Acl"],
        arrows: [["master", "t", .3, "Acl", "r", .5, "CreateImage", M2A]],
        d: ["cubefs 只在 distribution_scope 里随机挑一个可调度节点构建。", "模板 rootfs 和内存镜像放在共享卷上，B 不重复构建。"] },
      cubefsTplWrite,
      { k: "上传", time: "后台", on: ["Awrite", "minio"],
        arrows: [["Awrite", "b", .5, "minio", "t", .28, "PUT"]],
        d: ["staging 块上传到对象存储之后，B 才能读到模板数据。", "元数据在上传完成前就已经对所有挂载可见。"] },
      { k: "B 接管", time: "copy_file_range", on: ["Bcl", "Bjfs", "redis", "Ajfs"],
        arrows: [["Bcl", "b", .35, "Bjfs", "t", .55, "copy_file_range rootfs"], ["Bcl", "b", .75, "Bjfs", "t", .35, "copy_file_range 内存"]],
        d: ["B 第一次用这个模板时，把 A 的 rootfs 和内存镜像各 copy_file_range 成 B 自己的 volume。", "只复制元数据，每个节点只接管一次。"] },
      { k: "B 映射", time: "MAP_PRIVATE", on: ["Bvm", "Bjfs", "Bread", "minio"],
        arrows: [["Bjfs", "t", .55, "Bvm", "b", .3, "MAP_PRIVATE"], ["minio", "t", .55, "Bread", "b", .5, "冷 GET"]],
        d: ["B 以 MAP_PRIVATE 映射接管后的内存镜像。", "B 的读缓存是冷的，缺页从对象存储进入 jfs-cache/*/raw。"] },
    ] },
    { steps: [
      { k: "克隆 rootfs", time: "copy_file_range", on: ["Bcl", "Ajfs", "Bjfs", "redis", "fa-src", "fa-sb", "fb-src", "fb-sb"],
        files: [jfsFileCol(0, "fa-src", "模板 rootfs", "tpl-*-rootfs"), jfsFileCol(1, "fa-sb", "沙箱 rootfs", "sb-*-rootfs", "", true), jfsFileCol(0, "fb-src", "模板 rootfs", "tpl-*-rootfs", "B"), jfsFileCol(1, "fb-sb", "沙箱 rootfs", "sb-*-rootfs", "B", true)],
        arrows: [["fa-src", "r", .5, "fb-src", "l", .5, "同一文件", 0, -18], ["Bcl", "b", .08, "fb-sb", "l", .25, "copy_file_range", -20, 6], ["Bjfs", "b", .4, "redis", "t", .7, "元数据"]],
        d: ["B 用 copy_file_range 从模板 rootfs 克隆出沙箱 rootfs，只复制 slice 引用。", "A、B 挂载看到同一组文件。内存这一步不复制。"] },
      { k: "映射内存", time: "MAP_PRIVATE", on: ["Bvm", "Ajfs", "Bjfs", "fa-mem", "fb-mem"],
        files: [jfsFileCol(0, "fa-mem", "模板内存", "tpl-*-memory"), jfsFileCol(0, "fb-mem", "模板内存", "tpl-*-memory", "B")],
        arrows: [["fa-mem", "r", .5, "fb-mem", "l", .5, "同一文件", 0, -18], ["fb-mem", "r", .2, "Bvm", "l", .85, "MAP_PRIVATE"]],
        d: ["A、B 挂载上是同一份模板内存，这一步不复制它。", "B 的 VMM 直接 MAP_PRIVATE 映射。guest 写页变成匿名脏页，写盘写进上一步那个沙箱 rootfs。"] },
      { k: "冷读", time: "B 的读缓存", on: ["minio", "Bread", "Bjfs", "Bvm"],
        arrows: [["minio", "t", .6, "Bread", "b", .5, "GET"], ["Bjfs", "t", .6, "Bvm", "b", .25, "缺页"]],
        d: ["B 本地没有这些块，缺页从对象存储拉进 B 的读缓存。", "A 上的读缓存不会跟着沙箱过来。"] },
      { k: "连 guest agent", time: "probe 才返回", on: ["Bvm", "Bcl", "Bjfs", "Bread", "minio"],
        arrows: [["Bvm", "l", .3, "Bcl", "r", .3, "连 agent", [640, 44]], ["Bcl", "r", .75, "Bvm", "l", .75, "健康检查", [860, 148]], ["Bvm", "b", .45, "Bread", "t", .35, "缺页"], ["minio", "t", .6, "Bread", "b", .5, "GET"]],
        d: ["shim 经 vsock 连上已经在跑的 agent。", "Cubelet 对 guest 做健康检查，代码沙箱探的是 envd 的 /health。刚恢复的 guest 要先缺进检查碰到的页才能应答，这些缺页才变成对象存储 GET。API 要等检查成功才返回。"] },
    ] },
    { steps: [
      { k: "A 磁盘", time: "排空再克隆", on: ["Avm", "Acl", "Ajfs", "Awrite", "redis", "f-live", "f-root"],
        files: [jfsFileCol(0, "f-live", "沙箱 rootfs", "sb-*-rootfs", "", true), jfsFileCol(1, "f-root", "暂停 rootfs", "暂停包", "", true)],
        arrows: [["Avm", "r", .9, "f-live", "l", .2, "排空 virtio-blk"], ["Ajfs", "l", .42, "Awrite", "r", .55, "staging"], ["Acl", "b", .08, "f-root", "l", .25, "copy_file_range", -20, 6], ["Ajfs", "b", .55, "redis", "t", .45, "元数据"]],
        d: ["先排空 virtio-blk 里还在飞的请求，这些写落到正在用的沙箱 rootfs，并进入写 staging。", "再从这份沙箱 rootfs copy_file_range 出暂停 rootfs，只复制 slice 引用。"] },
      { k: "A 内存", time: "克隆再写脏页", on: ["Avm", "Acl", "Ajfs", "Awrite", "redis", "f-base", "f-mem"],
        files: [jfsFileCol(0, "f-base", "内存底稿", "正在映射"), jfsFileCol(1, "f-mem", "暂停内存", "暂停包", "", true)],
        arrows: [["Acl", "b", .5, "f-base", "t", .5, "copy_file_range"], ["Avm", "b", .93, "f-mem", "l", .25, "写回脏页", -28, 8], ["Ajfs", "l", .78, "Awrite", "r", .4, "staging"]],
        d: ["先 copy_file_range 内存底稿，只复制引用，这时还没有这次的脏页。", "再扫描页表拿到脏页。", "脏页按 1 MiB 窗口写回暂停内存，进入 A 的写 staging。元数据提交后暂停返回。"] },
      { k: "上传并 ready", time: "才允许跨机", on: ["Awrite", "minio", "Acl", "master"],
        arrows: [["Awrite", "b", .5, "minio", "t", .3, "PUT"], ["Acl", "r", .65, "master", "t", .45, "ready", A2M]],
        d: ["B 能看到共享元数据，但相关块还在 A 的 staging 时不能安全读。", "CubeMaster 收到 ready 之后才允许把这个暂停包调度到 B。"] },
      { k: "调度到 B", time: "跨机恢复", on: ["master", "Bcl"],
        arrows: [["master", "t", .7, "Bcl", "l", .5, "恢复到 B", [600, 340]]],
        d: ["A 不可用时 CubeMaster 选择 B。", "B 开始导入，不直接打开 A 的暂停包文件来跑 VM。"] },
      { k: "B 导入", time: "copy_file_range", on: ["Bcl", "Bjfs", "Ajfs", "redis", "f-root", "f-mem"],
        files: [jfsFileCol(0, "f-root", "暂停 rootfs", "B 的文件", "B", true), jfsFileCol(1, "f-mem", "暂停内存", "B 的文件", "B", true)],
        arrows: [["Bcl", "b", .5, "f-root", "t", .5, "copy_file_range rootfs"], ["Bcl", "b", .08, "f-mem", "l", .25, "copy_file_range 内存", -16, 8]],
        d: ["B 从 A 的暂停包私有导入 rootfs 和内存镜像。", "copy_file_range 只复制 slice 引用，B 得到自己的两份文件。"] },
      { k: "B 恢复", time: "MAP_PRIVATE", on: ["Bvm", "Bjfs", "Bread", "minio", "f-mem"],
        files: [jfsFileCol(0, "f-mem", "暂停内存", "直接映射", "B", true)],
        arrows: [["f-mem", "r", .2, "Bvm", "l", .85, "MAP_PRIVATE"], ["minio", "t", .6, "Bread", "b", .5, "GET"]],
        d: ["B 以 MAP_PRIVATE 映射导入后的内存镜像。", "B 的读缓存是冷的，缺页按需从对象存储进入 raw。"] },
    ] },
  ],
};


export { BOX, FRAMES, FLOWS, archSVG, layoutArrowLabels }
