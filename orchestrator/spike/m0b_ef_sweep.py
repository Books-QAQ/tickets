# -*- coding: utf-8 -*-
"""M0-B 补充：HNSW 检索参数 ef 的召回/延迟曲线（用于固定 §16.1 的参数取值）

只读已建好的 collection，不重建索引。
用法: .venv/Scripts/python.exe spike/m0b_ef_sweep.py [collection]
"""
import statistics
import sys
import time

import numpy as np
from qdrant_client import QdrantClient, models

DIM = 1024
TOPK = 50
N_Q = 50
COLLECTION = sys.argv[1] if len(sys.argv) > 1 else "m0_plain"

client = QdrantClient(host="localhost", grpc_port=16334, prefer_grpc=True, timeout=120)
rng = np.random.default_rng(11)
qs = rng.standard_normal((N_Q, DIM), dtype=np.float32)
qs /= np.linalg.norm(qs, axis=1, keepdims=True)

filt = models.Filter(must=[
    models.FieldCondition(key="category", match=models.MatchValue(value="refund")),
    models.FieldCondition(key="visibility", match=models.MatchValue(value="public")),
    models.FieldCondition(key="capability", match=models.MatchValue(value="supported")),
])


def pct(vals, p):
    vals = sorted(vals)
    return vals[min(len(vals) - 1, int(len(vals) * p / 100.0))]


def run(flt, label):
    print(f"\n[{label}]")
    # 精确结果做基准
    exact = []
    for i in range(N_Q):
        r = client.query_points(COLLECTION, query=qs[i].tolist(), limit=TOPK, query_filter=flt,
                                search_params=models.SearchParams(exact=True))
        exact.append({p.id for p in r.points})
    for ef in (64, 128, 256, 512):
        lat, rec = [], []
        for i in range(N_Q):
            t = time.perf_counter()
            r = client.query_points(COLLECTION, query=qs[i].tolist(), limit=TOPK, query_filter=flt,
                                    search_params=models.SearchParams(hnsw_ef=ef))
            lat.append((time.perf_counter() - t) * 1000.0)
            got = {p.id for p in r.points}
            rec.append(len(got & exact[i]) / max(1, len(exact[i])))
        print(f"  ef={ef:<4d} 召回={statistics.mean(rec)*100:5.1f}%  "
              f"P50={pct(lat,50):5.2f}ms  P95={pct(lat,95):5.2f}ms")


print(f"collection={COLLECTION}  查询数={N_Q}  top-k={TOPK}  （精确检索为基准）")
run(None, "无过滤")
run(filt, "过滤(category=refund & visibility=public & capability=supported)")
