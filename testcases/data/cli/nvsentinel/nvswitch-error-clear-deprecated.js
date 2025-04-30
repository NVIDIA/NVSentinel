/**
 * Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

const nodename = "your_node_name";

const query = {
  "healthevent.agent": "nvswitch-health-monitor",
  "healthevent.componentclass": "nvswitch",
  "healthevent.checkname": "NvswitchErrorFromKmsgWatch",
  "healthevent.nodename": nodename
};


const latestDocCursor = db.HealthEvents.find(query).sort({ createdAt: -1 }).limit(1);
const latestDoc = latestDocCursor.hasNext() ? latestDocCursor.next() : null;

if (!latestDoc) {
  print("No document found matching the criteria.");
} else {
  print("Latest document retrieved:");
  printjson(latestDoc);

  if (latestDoc.healthevent && typeof latestDoc.healthevent === 'object') {
    delete latestDoc._id;

    latestDoc.healthevent.isfatal = false;
    latestDoc.healthevent.ishealthy = true;

    latestDoc.createdAt = new Date();

    const insertResult = db.HealthEvents.insertOne(latestDoc);

    if (insertResult.acknowledged) {
      print("New document inserted successfully with _id: " + insertResult.insertedId);
    } else {
      print("Failed to insert the new document.");
    }
  } else {
    print("Error: The 'healthevent' field is missing or not an object in the retrieved document.");
  }
}
