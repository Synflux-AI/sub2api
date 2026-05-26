-- 为 Ops 告警增加 Lark 推送支持。
-- 与 notify_email / email_sent 配对：
--   notify_lark：规则是否选择推送到 Lark（默认 false，保留向后兼容）
--   lark_sent：事件是否已成功推送到 Lark，避免重复发送

ALTER TABLE ops_alert_rules
    ADD COLUMN IF NOT EXISTS notify_lark BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE ops_alert_events
    ADD COLUMN IF NOT EXISTS lark_sent BOOLEAN NOT NULL DEFAULT false;
