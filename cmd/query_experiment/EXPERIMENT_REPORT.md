# Query Experiment 最終實驗報告

本報告摘要已完成的單次方法驗證 run。不可變的 Concept JSONL 是實驗輸入；raw JSON/JSONL receipts 是可機器讀取的稽核與重播證據；本 Markdown 只保留摘要、量化結果與因果解讀。

## 1. 實驗問題、範圍與非目標

問題是：在固定語料與固定選擇參數下，三 Host 的輸入、輸出、拒絕、排序與稽核證據是否足以定位方法鏈路的失效位置，並比較小型 prompt/model matrix 的可觀測結果。

範圍是 `lifestyle-criterion-v1`、兩個 expansion prompt、三個 runtime model ID、六個帶有不同意圖與標註的 case、每個組合一次 run。生產 Query Service 未改動，仍是 control。

非目標：不是生產品質證明、不是普遍模型優劣排名、不是全域 relevance ground truth、不是參數微調或 per-user 自我演化設計，也不以輸出數量代替相關性。

## 2. 實驗輸入與固定條件

- 36 attempts = 1 profile × 2 prompts × 3 runtime model IDs × 6 cases × 1 run。
- `selection_limit=10`、`exploration_slots=1`、`seed=7`。
- frozen corpus：428 rows；digest：`e65809eb76e8e49e1860a0a2660fcfa7576a8e8c6b68be7c457cb0030554ffdc`。
- 36 筆 results records；每次 8 個 receipt，共 288 個；無缺 receipt。
- 憑證值在 summary、results 與 receipts 中均未發現。

## 3. 現行 3 Hosts 與流程

1. **Query Expansion**：接收 raw query、criterion policy、prompt/model fixture，產生並驗證 `QueryPlan`；失敗時使用受控 deterministic fallback。
2. **Matching / Eligibility**：以 `QueryPlan` 對 frozen corpus 的 428 筆 Concept 做 required/excluded 合規判斷，保留 lexical field/term evidence 與 score；semantic criteria 在沒有 evaluator 時不可當作證據。
3. **Candidate Selection**：接收候選、`limit`、探索槽位與有效 seed，按 score、tie-break 與 exploration 規則輸出 selected identities。

流程是 Expansion → Matching/Eligibility → Selection；每個 attempt 另有 final identity 與八類 receipts。此 trusted-local three-host 結果不等於 production synthesis 或 citation quality。

## 4. Host 輸入／輸出與 privacy boundary

| Host | 主要輸入 | 主要輸出 | receipts 與 privacy boundary |
|---|---|---|---|
| Query Expansion | raw query、criterion policy、prompt ID、provider/model fixture | structured 或 fallback `QueryPlan`、validation、source、latency、usage | `expansion.input.json`、`expansion.output.json`；保存可稽核的 raw model response、plan/metadata、validation/fallback、latency 與 usage，但本文不嵌入 raw response |
| Matching / Eligibility | plan、corpus digest、428 筆 corpus 的匹配所需欄位與參數 | candidate identity、eligible/rejection、group evidence、semantic outcome、score | `matching.input.json`、`matching.output.json`；receipt 不含 Concept body 或 snippets |
| Candidate Selection | candidates、limit=10、exploration=1、seed=7 | decisions、final order、reason、score、exploration marker | `selection.input.json`、`selection.output.json`；只摘要決策，不列完整候選陣列 |
| Attempt envelope | case、variant、run、Host trace | final identities、outcome、receipt map | `request.json`、`final.json`；不含 credentials/secrets，本文也不展示 raw response、body、snippets |

## 5. 精確 8-file artifact layout

每個 `<variant-id>/<case-id>/run-1/` 恰有以下 8 個檔案：

```text
request.json
expansion.input.json
expansion.output.json
matching.input.json
matching.output.json
selection.input.json
selection.output.json
final.json
```

本 run 的 `artifacts/` 共 36 個 attempt 目錄、288 個 receipt 檔；另有根層 `summary.json` 與 `results.jsonl`。原始 JSON/JSONL 留在 fixture run 供機器稽核，本報告不複製其中的完整 payload。

## 6. 最終實驗設計

### Matrix

| 維度 | 固定／選項 |
|---|---|
| Profile | `lifestyle-criterion-v1` |
| Prompt | `minimal-v1`、`extended-v1` |
| Runtime model ID | `deepseek-v4-flash`、`deepseek-v4-pro`、`grok-4.6` |
| Cases | 6 個，每個 variant 各 1 attempt |
| Selection | limit 10、exploration 1、seed 7 |
| Corpus | 428 rows、固定 digest |

### 六個 case intent

`exact-boven`（精確實體）、`taipei-work-cafe`（地點＋適合工作＋咖啡廳）、`outdoor-water-play`（兒童戶外戲水）、`rainy-child-indoor`（雨天兒童室內）、`explicit-exclusion`（南港工作／閱讀並排除指定站點）、`unsupported-skiing`（台北市區全年天然雪戶外滑雪場，預期可能無法支持）。known-positive 與 forbidden labels 僅對有標註的 case 貢獻分母。

## 7. 最終量化結果

### Overall（36 attempts）

| 指標 | 結果 |
|---|---:|
| fallback | 0/36 |
| zero-result | 0/36 |
| under-5 | 0/36 |
| result count | min 7 / max 10 / mean 9.5556 |
| Recall@5 | 39/54 = 0.7222 |
| Recall@10 | 47/54 = 0.8704 |
| forbidden violation | 0/6 |
| receipts | 288/288，無缺漏 |

本次只有一個 run，因此 pairwise stability 與 exact replay 的比較分母都是 0；不把它們表述為測得的穩定度，也不把 summary output 當成品質證明。

### 六 variant totals

| Prompt | Model | Recall@5 | Recall@10 | fallback | forbidden | mean latency | p95 latency | recorded total tokens |
|---|---|---:|---:|---:|---:|---:|---:|---:|
| minimal-v1 | deepseek-v4-flash | 6/9=.6667 | 7/9=.7778 | 0/6 | 0/1 | 164.50ms | 189ms | 30296 |
| minimal-v1 | deepseek-v4-pro | **7/9=.7778** | **9/9=1** | 0/6 | 0/1 | **162.33ms** | 217ms | **29257** |
| minimal-v1 | grok-4.6 | 6/9=.6667 | 7/9=.7778 | 0/6 | 0/1 | 50101.67ms | 79309ms | 21669 |
| extended-v1 | deepseek-v4-flash | 7/9=.7778 | 8/9=.8889 | 0/6 | 0/1 | 162.67ms | 194ms | 61313 |
| extended-v1 | deepseek-v4-pro | 7/9=.7778 | 8/9=.8889 | 0/6 | 0/1 | 170.33ms | 231ms | 51948 |
| extended-v1 | grok-4.6 | 6/9=.6667 | 8/9=.8889 | 0/6 | 0/1 | 69160.00ms | 81757ms | 28719 |

Recall 分母是六 case 中有 known positives 的五個 case、共 9 個 positive observations per variant；`unsupported-skiing` 沒有 known-positive 分母。extended DeepSeek 兩個 variant 的 Recall@5 都是 7/9，但 Recall@10 降為 8/9，recorded totals 為 flash 61313、pro 51948，沒有觀察到相對 minimal-pro 的品質增益。Grok 沒有品質優勢；其 mean recorded model latency 為 minimal 50.1s、extended 69.16s。跨 provider 的 token accounting 可能不同，token 數不作直接成本結論。

## 8. 候選 baseline 與狀態

`minimal-v1 × deepseek-v4-pro` 是這個小型 matrix 中最強候選：Recall@5 7/9=.7778、Recall@10 9/9=1、fallback 0、forbidden 0/1、mean latency 162.33ms、p95 217ms、recorded total tokens 29257。

它只是本次 matrix 的 candidate，不是普遍模型優越性，也不是已接受的 production baseline；尚未通過下述 unsupported/no-evidence、semantic evaluator、label coverage 與 stability 要求。

## 9. 一條 good path 與一條 bad path

### Good path：`outdoor-water-play` × minimal-pro

1. **Expansion**：產生可驗證的 structured plan，保留兒童、戶外、戲水等意圖。
2. **Matching / Eligibility**：428 筆皆為 eligible（無硬性 required gate）；lexical evidence 產生分數差異化，且兩個 known positives 都有足夠高的 score 與 rank。
3. **Selection**：在 limit 10、exploration 1、seed 7 下輸出 10 筆；兩個 known positives 均在前 5，該 attempt Recall@5=2/2、Recall@10=2/2。

這代表 trace 能把「plan 有效、候選可分數化、正例進入前段」連起來；不代表該 case 的完整 relevance 已被證明。

### Bad path：`unsupported-skiing` × minimal-pro

1. **Expansion 輸出**：valid structured plan；包含 required location、preferred 的滑雪／戶外／全年天然雪條件，以及 discovery goal。語義 evaluator 未連接，semantic outcome 不可用。
2. **Matching / Eligibility 輸出**：428 input rows；197 eligible、231 因 required location 不匹配而拒絕；eligible candidates 的 lexical score 仍只有 0 或 1，未提供能證明「台北市區全年天然雪戶外滑雪」存在的語義證據。
3. **Selection 輸出**：選 10 筆，9 筆 score 1、1 筆 score 0 exploration；結果看起來與滑雪意圖無關。

因此第一個可行動失效是 Expansion 對 unsupported/no-evidence 的角色與契約不足，而非單純 Selector 排序 bug；其後又被沒有 semantic evaluator 與 fill-oriented Selection 放大。`eligible=197` 不表示 197 筆都相關；zero-result/under-5=0 也不能在此情況下認證 relevance。

## 10. known-positive miss 歸因與對 Hosts 的意義

在 54 個 known-positive observations 中：前 5 命中 39、落在第 6–10 名 8、eligible 但未選入 7、matching candidate missing 0、eligibility rejection 0。

這表示本次 labeled-case misses 發生在「排序／選擇」之後段，而不是正例在 Matching 時被錯誤淘汰。Expansion 決定 score evidence；因此不能把因果壓平為 Selector-only 問題：Selector 暴露並放大 ranking/selection 的缺口，但 Expansion 的 plan 與 evidence contract 仍是整條品質鏈的上游前提。

## 11. current external-vs-fixed boundary

### 11.1 目前實驗性的 external fixtures

model fixture、prompt fixture、criterion policy profile、cases/corpus、CLI run knobs（limit、exploration、seed）是本次實驗控制項，不等於未來所有 per-user settings。

### 11.2 預期固定的平台／程式契約

選定的 model/provider/credential 與 decoding baseline、`QueryPlan` schema、system/prompt contract、required/excluded semantics、validation/fallback、matching fields 與 semantic outcomes、scoring/selection/tie-break/exploration，應是平台／程式固定的契約。精確存放位置（code 或 platform-owned config）可稍後決定；它們不是 per-user self-evolution inputs。

### 11.3 未實作的未來可編輯描述文字 seam

未來可考慮 Project Description 與 User Expansion Profile：只放自然語言偏好、詞彙、正／負例與理由；不放 if/then、weights、gates、algorithms 或 credentials。未來模式可為 manual、suggested diff，或明確 opt-in 的 managed updates，並要求 revision/diff/undo/regression。此 feature 延後，直到有接近最佳的固定 baseline。

## 12. acceptance interpretation 與 gaps

- 必須補明 unsupported/no-evidence contract：無證據時應能表達不可支持，而不是以候選數量填滿。
- 需明確定義 tie ranking 與 exploration 對 labeled miss 的影響。
- semantic evaluator 尚未連接；請區分 semantic unavailable 與 semantic pass/fail。
- labels 僅覆蓋六 case 中的有限正例，不能推論全域品質。
- 單次 run 沒有 stability 或 exact replay 的有效分母。
- result count 只代表輸出數量，不代表 relevance。

目前不應先做細微參數調校；先固定上述 Host contracts、evaluator 與 label/fixture 定義，才有可解釋的品質比較。

## 13. artifacts 與最終 verdict

證據根目錄：`/Users/rayer/Develop/llm-wiki-bff-query-fixtures/runs/2026-08-14-method-validation-v1/`，包含 `summary.json`、`results.jsonl`、`cases.jsonl` 與 `artifacts/`。frozen fixture metadata：`/Users/rayer/Develop/llm-wiki-bff-query-fixtures/dev-demo-lifestyle-g_fef0cfbe663262c34a263c2e029cdaeb/fixture.json`。

最終 verdict：本次 run 成功驗證三 Host 的可追蹤 artifact 鏈路、量化分母與階段式診斷；`minimal-v1 × deepseek-v4-pro` 是小型 matrix 中的候選，但不是 production-ready baseline。下一個必要門檻是建立 unsupported/no-evidence 行為、semantic evaluator、tie/selection 規則與更充分 labels；在此之前，不把輸出數量、單次命中或 summary 一致性解讀為生產品質。
