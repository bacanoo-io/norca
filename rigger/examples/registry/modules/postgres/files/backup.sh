#!/bin/sh
# Dump every database, upload it, remove the dumps older than KEEP_DAYS.
# A plain file of the module: backup.norca.hcl reads it with file(), nothing in it
# is filled in by Rigger; its settings come from the environment of the task.
set -eu

stamp=$(date -u +%Y%m%dT%H%M%SZ)
dump="/tmp/${stamp}.sql.gz"

pg_dumpall | gzip > "$dump"
aws --endpoint-url "$S3_ENDPOINT" s3 cp "$dump" "s3://${S3_BUCKET}/${PREFIX}/${stamp}.sql.gz"

cutoff=$(date -u -d "-${KEEP_DAYS} days" +%Y%m%dT%H%M%SZ)
aws --endpoint-url "$S3_ENDPOINT" s3 ls "s3://${S3_BUCKET}/${PREFIX}/" | while read -r _ _ _ name; do
  [ "${name%.sql.gz}" \< "$cutoff" ] && aws --endpoint-url "$S3_ENDPOINT" s3 rm "s3://${S3_BUCKET}/${PREFIX}/${name}"
done
