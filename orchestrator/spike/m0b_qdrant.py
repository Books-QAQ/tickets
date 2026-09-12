# -*- coding: utf-8 -*-
"""M0-B 依赖 spike：Qdrant 十万级【过滤式 ANN】实测（含 int8 标量量化前后对比）

对应技术设计文档 ADR-10 与 §7.7：
  - 10⁵ × 1024 维（BGE-M3 / Qwen 量级），单位化向量 + COSINE
  - 过滤条件贴合真实设计：category(7 类) + visibility + capability + scope_ref
  - 输出：建索引耗时、灌库吞吐、P50/P95 延迟、ANN 相对 exact 的召回
  - 对比：plain vs int8 标量量化

用法（在 orchestrator/ 下，需 Qdrant 已启动）:
    .venv/Scripts/python.exe spike/m0b_qdrant.py plain
    .venv/Scripts/python.exe spike/m0b_qdrant.py int8
    .venv/Scripts/python.exe spike/m0b_qdrant.py info
    .venv/Scripts/python.exe spike/m0b_qdrant.py drop
"""
import os
import statistics
import sys
import time

import numpy as np
from qdrant_client import QdrantClient, models

DIM = 1024
N = 100_000
TOPK = 50
N_QUERIES = 200
BATCH = 1000

HOST = os.environ.get("QDRANT_HOST", "localhost")
GRPC_PORT = int(os.environ.get("QDRANT_GRPC_PORT", "16334"))

CATEGORIES = ["booking", "order", "refund", "account", "travel", "app", "policy"]
CAPABILITIES = ["supported", "roadmap", "industry"]


def client() -> QdrantClient:
    return QdrantClient(host=HOST, grpc_port=GRPC_PORT, prefer_grpc=True, timeout=120)


def rand_vecs(n: int, rng: np.random.Generator) -> np.ndarray:
    v = rng.standard_normal((n, DIM), dtype=np.float32)
    v /= np.linalg.norm(v, axis=1, keepdims=True)
    return v


def payload_for(i: int, rng: np.random.Generator) -> dict:
    # 注意：scope_kind 与 scope_ref 必须一致（M0 第一次跑时两者取模条件互斥，
    # 导致窄过滤命中 0 —— 造数据 bug，已修）
    if i % 10 < 3:
        scope_kind, scope_ref = "station", f"station_{(i // 3) % 300}"
    else:
        scope_kind, scope_ref = "general", None
    return {
        "chunk_id": i,
        "category": CATEGORIES[i % len(CATEGORIES)],
        "visibility": "authenticated" if i % 10 == 0 else "public",
        "capability": CAPABILITIES[i % 10 % 3] if i % 3 else "supported",
        "scope_kind": scope_kind,
        "scope_ref": scope_ref,
        "form": "prose" if i % 2 else "qa",
        "expire_at": None if i % 10 else "2027-01-01",
    }


def create(client_: QdrantClient, name: str, quantize: bool):
    if client_.collection_exists(name):
        client_.delete_collection(name)
    q = None
    if quantize:
        q = models.ScalarQuantization(
            scalar=models.ScalarQuantizationConfig(
                type=models.ScalarType.INT8, quantile=0.99, always_ram=True
            )
        )
    t0 = time.perf_counter()
    client_.create_collection(
        collection_name=name,
        vectors_config=models.VectorParams(size=DIM, distance=models.Distance.COSINE),
        hnsw_config=models.HnswConfigDiff(m=16, ef_construct=100),
        quantization_config=q,
    )
    for field, schema in [
        ("category", models.PayloadSchemaType.KEYWORD),
        ("visibility", models.PayloadSchemaType.KEYWORD),
        ("capability", models.PayloadSchemaType.KEYWORD),
        ("scope_kind", models.PayloadSchemaType.KEYWORD),
        ("scope_ref", models.PayloadSchemaType.KEYWORD),
        ("form", models.PayloadSchemaType.KEYWORD),
    ]:
        client_.create_payload_index(name, field_name=field, field_schema=schema)
    print(f"[create] {name} 就绪（int8={'on' if quantize else 'off'}），耗时 {time.perf_counter()-t0:.1f}s")


def load(client_: QdrantClient, name: str):
    rng = np.random.default_rng(42)
    t0 = time.perf_counter()
    for start in range(0, N, BATCH):
        n = min(BATCH, N - start)
        vecs = rand_vecs(n, rng)
        points = [
            models.PointStruct(
                id=start + k,
                vector=vecs[k].tolist(),
                payload=payload_for(start + k, rng),
            )
            for k in range(n)
        ]
        client_.upsert(name, points=points, wait=False)
        if (start // BATCH) % 20 == 0:
            done = start + n
            print(f"         已灌 {done}/{N}（{done/(time.perf_counter()-t0):.0f} 点/秒）")
    info = client_.get_collection(name)
    print(f"[load] 完成：{info.points_count} 点，总耗时 {time.perf_counter()-t0:.1f}s")


def wait_indexed(client_: QdrantClient, name: str):
    while True:
        info = client_.get_collection(name)
        if info.status == models.CollectionStatus.GREEN and info.indexed_vectors_count:
            print(f"[load] 索引就绪：indexed={info.indexed_vectors_count}")
            return
        time.sleep(2)


def pct(vals, p):
    vals = sorted(vals)
    return vals[min(len(vals) - 1, int(len(vals) * p / 100.0))]


def bench(client_: QdrantClient, name: str, label: str, flt=None, exact=False):
    rng = np.random.default_rng(7)
    qs = rand_vecs(N_QUERIES, rng)
    # 显式固定检索参数，保证结果可复现（hnsw_ef 取 Qdrant 常见默认值 128）
    params = models.SearchParams(hnsw_ef=None if exact else 128, exact=exact)
    lat, hits = [], []
    # 预热
    for i in range(10):
        client_.query_points(name, query=qs[i].tolist(), limit=TOPK, query_filter=flt, search_params=params)
    for i in range(N_QUERIES):
        t = time.perf_counter()
        r = client_.query_points(name, query=qs[i].tolist(), limit=TOPK, query_filter=flt, search_params=params)
        lat.append((time.perf_counter() - t) * 1000.0)
        hits.append(len(r.points))
    print(f"[bench] {label}")
    print(f"        P50={pct(lat,50):.2f}ms  P95={pct(lat,95):.2f}ms  "
          f"mean={statistics.mean(lat):.2f}ms  max={max(lat):.2f}ms  平均命中={statistics.mean(hits):.1f}")
    return lat


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        return 2
    cmd = sys.argv[1]
    c = client()

    if cmd == "drop":
        for n in ("m0_plain", "m0_int8"):
            if c.collection_exists(n):
                c.delete_collection(n)
                print(f"[drop] 已删除 {n}")
        return 0

    if cmd == "info":
        for n in ("m0_plain", "m0_int8"):
            if c.collection_exists(n):
                i = c.get_collection(n)
                print(f"[info] {n}: points={i.points_count} indexed={i.indexed_vectors_count} "
                      f"status={i.status} quant={i.config.quantization_config is not None}")
        return 0

    if cmd not in ("plain", "int8"):
        print(__doc__)
        return 2

    name = f"m0_{cmd}"
    create(c, name, quantize=(cmd == "int8"))
    load(c, name)
    wait_indexed(c, name)

    filt_wide = models.Filter(must=[
        models.FieldCondition(key="category", match=models.MatchValue(value="refund")),
        models.FieldCondition(key="visibility", match=models.MatchValue(value="public")),
        models.FieldCondition(key="capability", match=models.MatchValue(value="supported")),
    ])
    filt_narrow = models.Filter(must=[
        models.FieldCondition(key="scope_kind", match=models.MatchValue(value="station")),
        models.FieldCondition(key="scope_ref", match=models.MatchValue(value="station_7")),
    ])

    bench(c, name, f"{cmd} | 无过滤 top-{TOPK}")
    bench(c, name, f"{cmd} | 过滤(category=refund&visibility=public&capability=supported)", flt=filt_wide)
    bench(c, name, f"{cmd} | 过滤(scope_ref=station_7) 窄过滤", flt=filt_narrow)

    # ANN vs exact 召回（20 条查询，看 HNSW 在过滤下的召回损失）
    rng = np.random.default_rng(99)
    qs = rand_vecs(20, rng)
    same = 0
    for i in range(20):
        a = c.query_points(name, query=qs[i].tolist(), limit=TOPK, query_filter=filt_wide)
        b = c.query_points(name, query=qs[i].tolist(), limit=TOPK, query_filter=filt_wide,
                           search_params=models.SearchParams(exact=True))
        a_ids = {p.id for p in a.points}
        b_ids = {p.id for p in b.points}
        same += len(a_ids & b_ids) / max(1, len(b_ids))
    print(f"[bench] {cmd} | HNSW vs exact 召回（过滤条件下，20 条查询平均）= {same/20*100:.1f}%")

    i = c.get_collection(name)
    print(f"[info] {name}: points={i.points_count} indexed={i.indexed_vectors_count}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
