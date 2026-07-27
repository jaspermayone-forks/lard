-- Remove the pre-subject-file record model. The KV/record view (records,
-- docs), the old consolidation marker (consolidated), and the unused
-- server-side watermarks table were superseded by the subject-file model in
-- migration 003: facts plus markdown subject files on disk.

DROP TABLE IF EXISTS records;
DROP TABLE IF EXISTS docs;
DROP TABLE IF EXISTS consolidated;
DROP TABLE IF EXISTS watermarks;
