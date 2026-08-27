{{/*
Mongosh --eval body for the in-cluster setup Job.
Hashed into the Job name so an index/script change creates a new Job.
*/}}
{{- define "mongodb-store.initEval" -}}
db = db.getSiblingDB('$MONGODB_DATABASE_NAME');

// Create collections if they don't exist
if (!db.getCollectionNames().includes('$MONGODB_COLLECTION_NAME')) {
  db.createCollection('$MONGODB_COLLECTION_NAME');
  print('Created collection: $MONGODB_COLLECTION_NAME');
} else {
  print('Collection already exists: $MONGODB_COLLECTION_NAME');
}

if (!db.getCollectionNames().includes('$MONGODB_TOKEN_COLLECTION_NAME')) {
  db.createCollection('$MONGODB_TOKEN_COLLECTION_NAME');
  print('Created collection: $MONGODB_TOKEN_COLLECTION_NAME');
} else {
  print('Collection already exists: $MONGODB_TOKEN_COLLECTION_NAME');
}

if (!db.getCollectionNames().includes('$MONGODB_MAINTENANCE_EVENT_COLLECTION_NAME')) {
  db.createCollection('$MONGODB_MAINTENANCE_EVENT_COLLECTION_NAME');
  print('Created collection: $MONGODB_MAINTENANCE_EVENT_COLLECTION_NAME');
} else {
  print('Collection already exists: $MONGODB_MAINTENANCE_EVENT_COLLECTION_NAME');
}

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

// Non-TTL indexes (MongoDB handles identical duplicates gracefully)
db.$MONGODB_MAINTENANCE_EVENT_COLLECTION_NAME.createIndex(
  { 'scheduledStartTime': 1 },
);
db.$MONGODB_MAINTENANCE_EVENT_COLLECTION_NAME.createIndex(
  { 'cspStatus': 1 },
);
db.$MONGODB_COLLECTION_NAME.createIndex({
  'healthevent.nodename': 1,
  'healthevent.entitiesimpacted.entitytype': 1,
  'healthevent.entitiesimpacted.entityvalue': 1,
  'healthevent.generatedtimestamp.seconds': 1
});
{{- if .Values.mongodb.tls.enabled }}
// Create X.509 users (TLS only)
var userExists = db.getSiblingDB('\$external').getUser('$MONGODB_APPLICATION_USER_DN');
if (userExists) {
  print('User already exists, skipping creation.');
} else {
  print('Creating new X.509 user...');
  db.getSiblingDB('\$external').runCommand({
    createUser: '$MONGODB_APPLICATION_USER_DN',
    roles: [{ role: 'readWrite', db: '$MONGODB_DATABASE_NAME' }]
  });
  print('X.509 user created successfully.');
}
var dgxcopsUserExists = db.getSiblingDB('\$external').getUser('$MONGODB_DGXCOPS_USER_DN');
if (dgxcopsUserExists) {
  print('Dgxcops user already exists, skipping creation.');
} else {
  print('Creating new dgxcops X.509 user...');
  db.getSiblingDB('\$external').runCommand({
    createUser: '$MONGODB_DGXCOPS_USER_DN',
    roles: [{ role: 'read', db: '$MONGODB_DATABASE_NAME' }]
  });
  print('Dgxcops X.509 user created successfully.');
}
{{- else }}
// Create SCRAM application user (non-TLS mode)
var scramUser = db.getUser('$MONGODB_SCRAM_APP_USERNAME');
if (scramUser) {
  print('SCRAM application user already exists, updating password...');
  db.changeUserPassword('$MONGODB_SCRAM_APP_USERNAME', '$MONGODB_SCRAM_APP_PASSWORD');
  print('SCRAM application user password updated.');
} else {
  print('Creating SCRAM application user...');
  db.createUser({
    user: '$MONGODB_SCRAM_APP_USERNAME',
    pwd: '$MONGODB_SCRAM_APP_PASSWORD',
    roles: [{ role: 'readWrite', db: '$MONGODB_DATABASE_NAME' }]
  });
  print('SCRAM application user created successfully.');
}
{{- end }}
{{- end }}
