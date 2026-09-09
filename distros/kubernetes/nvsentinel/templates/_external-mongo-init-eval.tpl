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
