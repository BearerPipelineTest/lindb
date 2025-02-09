/*
Licensed to LinDB under one or more contributor
license agreements. See the NOTICE file distributed with
this work for additional information regarding copyright
ownership. LinDB licenses this file to you under
the Apache License, Version 2.0 (the "License"); you may
not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
 
Unless required by applicable law or agreed to in writing,
software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
KIND, either express or implied.  See the License for the
specific language governing permissions and limitations
under the License.
*/
import { Card } from "@douyinfe/semi-ui";
import { MasterNodeView } from "@src/components";
import { Master } from "@src/models";
import { exec } from "@src/services";
import React, { useCallback, useEffect, useState } from "react";

export default function MasterView() {
  const [master, setMaster] = useState<Master>();
  const [loading, setLoading] = useState(true);
  const getCurrentMaster = useCallback(async () => {
    try {
      setLoading(true);
      const currentMaster = await exec<Master>({ sql: "show master" });
      setMaster(currentMaster);
    } catch (err) {
      console.log(err);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    getCurrentMaster();
  }, [getCurrentMaster]);

  return (
    <>
      <Card
        title="Master"
        headerStyle={{ padding: 12 }}
        bodyStyle={{ padding: 12 }}
        loading={loading}
      >
        <MasterNodeView master={master} />
      </Card>
    </>
  );
}
