-- 000005_add_cs_tables.down.sql
-- 逆序删除（先删有外键引用的子表）
DROP TABLE IF EXISTS cs_feedback;
DROP TABLE IF EXISTS support_tickets;
DROP TABLE IF EXISTS cs_messages;
DROP TABLE IF EXISTS cs_conversations;
DROP TABLE IF EXISTS cs_capabilities;
DROP TABLE IF EXISTS kb_chunks;
DROP TABLE IF EXISTS kb_documents;
