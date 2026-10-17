-- 网关设置：Kiro 计量算法（token 计数口径）。
-- official = Anthropic 参考 BPE 精确计数（默认，行为与历史版本一致）；
-- charclass / weighted = 轻量启发式估算口径。

INSERT INTO settings (key, value, updated_at)
VALUES ('gateway_kiro_tokenizer', 'official', NOW())
ON CONFLICT (key) DO NOTHING;
