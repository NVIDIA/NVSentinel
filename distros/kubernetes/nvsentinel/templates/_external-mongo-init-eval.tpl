{{/*
Mongosh --eval body for the external Mongo setup Job.
Hashed into the Job name so an index/script change creates a new Job.
*/}}
{{- define "nvsentinel.externalMongoInitEval" -}}
{{- $authMechanism := "scram" }}
{{- if and .Values.global.datastore.auth .Values.global.datastore.auth.mechanism }}
{{- $authMechanism = .Values.global.datastore.auth.mechanism }}
{{- end }}
db = db.getSiblingDB('$MONGODB_DATABASE_NAME');

// Create collections if they don't exist
['$MONGODB_COLLECTION_NAME',
 '$MONGODB_TOKEN_COLLECTION_NAME',
 '$MONGODB_MAINTENANCE_EVENT_COLLECTION_NAME'].forEach(function(col) {
  if (!db.getCollectionNames().includes(col)) {
    db.createCollection(col);
    print('Created collection: ' + col);
  } else {
    print('Collection already exists: ' + col);
  }
});

// createIndex if missing; collMod if expireAfterSeconds changed
function ensureTTL(collName, field) {
  var raw = '$MONGODB_COLLECTION_EXPIRY_SECONDS';
  var secs = Number(raw);
  if (raw === '' || !Number.isInteger(secs) || secs < 0) {
    throw new Error('MONGODB_COLLECTION_EXPIRY_SECONDS must be a non-negative integer, got ' + JSON.stringify(raw));
  }
  var key = {};
  key[field] = 1;
  var existing = db.getCollection(collName).getIndexes().find(function(idx) {
    return idx.key && idx.key[field] === 1 && Object.keys(idx.key).length === 1;
  });
  if (!existing) {
    db.getCollection(collName).createIndex(key, { expireAfterSeconds: secs });
    print('Created TTL index ' + collName + '.' + field + '=' + secs);
  } else if (existing.expireAfterSeconds != secs) {
    var res = db.runCommand({
      collMod: collName,
      index: { name: existing.name, expireAfterSeconds: secs }
    });
    if (res.ok !== 1) {
      throw new Error('collMod failed for ' + collName + ': ' + tojson(res));
    }
    print('Updated TTL index ' + collName + '.' + field + '=' + secs);
  } else {
    print('TTL index ' + collName + '.' + field + ' already ' + secs);
  }
}
ensureTTL('$MONGODB_COLLECTION_NAME', 'createdAt');
ensureTTL('$MONGODB_MAINTENANCE_EVENT_COLLECTION_NAME', 'actualEndTime');
db.$MONGODB_COLLECTION_NAME.createIndex({ 'createdAt': 1, '_id': 1 });
// Derived state and recovery history lookups, scoped to one rule and node.
db.$MONGODB_COLLECTION_NAME.createIndex({
  'healthevent.agent': 1,
  'healthevent.checkname': 1,
  'healthevent.nodename': 1,
  'createdAt': -1
}, { name: 'idx_health_events_analyzer_lookup' });
db.$MONGODB_COLLECTION_NAME.createIndex({
  'healthevent.agent': 1,
  'healthevent.componentclass': 1,
  'healthevent.checkname': 1,
  'healthevent.nodename': 1,
  'healthevent.version': 1,
  'createdAt': 1,
  '_id': 1
});
db.$MONGODB_MAINTENANCE_EVENT_COLLECTION_NAME.createIndex(
  { 'scheduledStartTime': 1 }
);
db.$MONGODB_MAINTENANCE_EVENT_COLLECTION_NAME.createIndex(
  { 'cspStatus': 1 }
);
db.$MONGODB_COLLECTION_NAME.createIndex({
  'healthevent.nodename': 1,
  'healthevent.entitiesimpacted.entitytype': 1,
  'healthevent.entitiesimpacted.entityvalue': 1,
  'healthevent.generatedtimestamp.seconds': 1
});
// Health-events-analyzer scopes every rule to one node and one time window.
// The index above is multikey on entitiesimpacted, so it emits several keys per
// document. These two are not multikey and emit one key per document, and they
// put the equality fields the rules filter on ahead of the time range.
// 16 of the 22 shipped rules filter on ishealthy.
db.$MONGODB_COLLECTION_NAME.createIndex({
  'healthevent.nodename': 1,
  'healthevent.ishealthy': 1,
  'healthevent.generatedtimestamp.seconds': 1
});
// MultipleRemediations is the only rule reading faultremediated, and the only
// one with a 7-day window, so it examines the most documents of any rule.
db.$MONGODB_COLLECTION_NAME.createIndex({
  'healthevent.nodename': 1,
  'healtheventstatus.faultremediated.value': 1,
  'healthevent.isfatal': 1,
  'healthevent.generatedtimestamp.seconds': 1
});

{{ include "nvsentinel.mongoIdempotencyIndexEval" . }}

{{- if eq $authMechanism "x509" }}
// X.509 user creation (only for x509 auth mechanism)
var appUserDN = '$MONGODB_APPLICATION_USER_DN';
var opsUserDN = '$MONGODB_DGXCOPS_USER_DN';

var userExists = db.getSiblingDB('\$external').getUser(appUserDN);
if (userExists) {
  print('App user already exists, skipping.');
} else {
  db.getSiblingDB('\$external').runCommand({
    createUser: appUserDN,
    roles: [{ role: 'readWrite', db: '$MONGODB_DATABASE_NAME' }]
  });
  print('App user created: ' + appUserDN);
}

var opsUserExists = db.getSiblingDB('\$external').getUser(opsUserDN);
if (opsUserExists) {
  print('Ops user already exists, skipping.');
} else {
  db.getSiblingDB('\$external').runCommand({
    createUser: opsUserDN,
    roles: [{ role: 'read', db: '$MONGODB_DATABASE_NAME' }]
  });
  print('Ops user created: ' + opsUserDN);
}
{{- end }}

print('MongoDB setup complete.');
{{- end }}

{{/*
The deployment platform connector's idempotency index, created by both setup
Jobs. Both evals run inside sh -c "...", so every $ meant for mongosh is \$.
*/}}
{{- define "nvsentinel.mongoIdempotencyIndexEval" -}}
// Unique idempotency key of the deployment platform connector. The index is
// partial, so events stored without a key are left alone; createIndex does
// nothing when the index already exists.
var idempotencyIndexName = 'healthevent_idempotency_key_unique';
var idempotencyKeyPath = 'healthevent.metadata.idempotencyKey';
var idempotencyKey = {};
idempotencyKey[idempotencyKeyPath] = 1;
var idempotencyFilter = {};
idempotencyFilter[idempotencyKeyPath] = { '\$exists': true };
var idempotencyOptions = { name: idempotencyIndexName, unique: true, partialFilterExpression: idempotencyFilter };
// The connector refuses an index with a collation, so the index must not
// inherit a collection default one. The option is set only then, because
// Amazon DocumentDB 5.0 rejects it.
var idempotencyCollection = (db.getCollectionInfos({ name: '$MONGODB_COLLECTION_NAME' })[0] || {}).options || {};
if (idempotencyCollection.collation && idempotencyCollection.collation.locale !== 'simple') {
  idempotencyOptions.collation = { locale: 'simple' };
}
try {
  db.$MONGODB_COLLECTION_NAME.createIndex(idempotencyKey, idempotencyOptions);
  print('Idempotency index ' + idempotencyIndexName + ' is in place');
} catch (e) {
  // Stored events share a key. Report it and go on, so the rest of the setup
  // still runs; the connector replicas stay not ready until the index exists.
  if (e.code !== 11000) {
    throw e;
  }
  print('ERROR: index ' + idempotencyIndexName + ' was not created because stored events share an idempotency key. Remove the extra events, then run the setup again. List the shared keys with: db.$MONGODB_COLLECTION_NAME.aggregate([{\$match: {\'' + idempotencyKeyPath + '\': {\$exists: true}}}, {\$group: {_id: \'\$' + idempotencyKeyPath + '\', n: {\$sum: 1}}}, {\$match: {n: {\$gt: 1}}}])');
}
{{- end }}
