-- Manual conversion of a backed-up v1 catalog. Never run by taper itself.
-- The connection must be outside a transaction. Use a writable copy of the
-- authenticated export; only install it after comparing every preserved value.
-- A single committed stream per batch is required. Unassigned/abandoned plans
-- and unresolved writes must be reviewed separately before running this script.
PRAGMA foreign_keys=OFF;
BEGIN IMMEDIATE;
CREATE TEMP TABLE migration_assert(ok INTEGER NOT NULL CHECK(ok=1));
-- Reject unknown settings and ambiguous on-tape identities instead of guessing.
INSERT INTO migration_assert SELECT CASE WHEN COUNT(*)=0 THEN 1 ELSE 0 END FROM media WHERE hardware_compression IS NULL;
INSERT INTO migration_assert SELECT CASE WHEN COUNT(*)=0 THEN 1 ELSE 0 END FROM batches b WHERE b.state!='committed' OR (SELECT COUNT(*) FROM replicas r WHERE r.batch_id=b.id AND r.state='committed')!=1;
INSERT INTO migration_assert SELECT CASE WHEN COUNT(*)=0 THEN 1 ELSE 0 END FROM replicas WHERE state!='committed' OR data_pos IS NULL OR blocks IS NULL OR end_pos IS NULL OR data_pos!=begin_pos+1 OR commit_pos!=data_pos+blocks OR end_pos!=commit_pos+2;
INSERT INTO migration_assert SELECT CASE WHEN COUNT(*)=0 THEN 1 ELSE 0 END FROM replicas r JOIN batches b ON b.id=r.batch_id WHERE blocks!=1+(b.logical_bytes-1)/1048512 OR b.logical_bytes<=0 OR b.files<=0;
INSERT INTO migration_assert SELECT CASE WHEN COUNT(*)=0 THEN 1 ELSE 0 END FROM replicas a JOIN replicas b ON a.media_id=b.media_id AND a.id<>b.id AND a.begin_pos<=b.begin_pos AND a.end_pos>b.begin_pos;
CREATE TABLE replica_sets(id TEXT PRIMARY KEY,name TEXT NOT NULL UNIQUE,key_id TEXT NOT NULL,key BLOB,hardware_compression INTEGER NOT NULL CHECK(hardware_compression IN (0,1)));
CREATE TABLE media_new(id TEXT PRIMARY KEY,replica_id TEXT NOT NULL REFERENCES replica_sets(id),label TEXT NOT NULL, note TEXT NOT NULL DEFAULT '',serial TEXT NOT NULL UNIQUE,total INTEGER NOT NULL,remaining INTEGER NOT NULL,sampled TEXT NOT NULL,state TEXT NOT NULL,key_id TEXT NOT NULL,key BLOB,hardware_compression INTEGER CHECK(hardware_compression IN (0,1)));
CREATE TABLE batches_new(id TEXT PRIMARY KEY,replica_id TEXT NOT NULL REFERENCES replica_sets(id),stream_id TEXT NOT NULL,seq INTEGER NOT NULL,name TEXT NOT NULL,source TEXT NOT NULL,created TEXT NOT NULL,rules TEXT NOT NULL,digest TEXT NOT NULL,logical_bytes INTEGER NOT NULL,files INTEGER NOT NULL,begin_pos INTEGER NOT NULL,data_pos INTEGER NOT NULL,commit_pos INTEGER NOT NULL,blocks INTEGER NOT NULL,end_pos INTEGER NOT NULL,encoded_bytes INTEGER,state TEXT NOT NULL,UNIQUE(replica_id,seq));
CREATE TABLE stage_new(id INTEGER PRIMARY KEY CHECK(id=1),replica_id TEXT REFERENCES replica_sets(id),source TEXT NOT NULL,identity TEXT NOT NULL,rules TEXT NOT NULL,plan_id TEXT REFERENCES batches(id));

-- Names are descriptive; IDs and keys are taken from existing tape evidence.
INSERT INTO replica_sets(id,name,key_id,key,hardware_compression)
 SELECT id,CASE WHEN (SELECT COUNT(*) FROM media x WHERE x.label=m.label)=1 THEN label ELSE label||' ('||id||')' END,key_id,key,hardware_compression FROM media m;
INSERT INTO media_new(id,replica_id,label,note,serial,total,remaining,sampled,state,key_id,key,hardware_compression)
 SELECT id,id,label,'',serial,total,remaining,sampled,state,key_id,key,hardware_compression FROM media;
INSERT INTO batches_new(id,replica_id,stream_id,seq,name,source,created,rules,digest,logical_bytes,files,begin_pos,data_pos,commit_pos,blocks,end_pos,encoded_bytes,state)
 SELECT b.id,r.media_id,r.id,ROW_NUMBER() OVER(PARTITION BY r.media_id ORDER BY r.begin_pos)-1,
 b.name,b.source,b.created,b.rules,b.digest,b.logical_bytes,b.files,r.begin_pos,r.data_pos,r.commit_pos,r.blocks,r.end_pos,r.confirmed_bytes,b.state
 FROM batches b JOIN replicas r ON r.batch_id=b.id AND r.state='committed';
INSERT INTO stage_new(id,replica_id,source,identity,rules,plan_id)
 SELECT id,(SELECT replica_id FROM batches_new WHERE id=stage.plan_id),source,identity,rules,plan_id FROM stage;
-- Copy then replace tables; do not rename old tables and redirect child FKs.
DROP TABLE stage;
DROP TABLE batches;
DROP TABLE media;
ALTER TABLE media_new RENAME TO media;
ALTER TABLE batches_new RENAME TO batches;
ALTER TABLE stage_new RENAME TO stage;
INSERT INTO migration_assert SELECT CASE WHEN COUNT(*)=0 THEN 1 ELSE 0 END FROM pragma_foreign_key_check;
DROP TABLE migration_assert;
COMMIT;
PRAGMA foreign_keys=ON;
