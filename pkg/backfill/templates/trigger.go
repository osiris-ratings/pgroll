// SPDX-License-Identifier: Apache-2.0

package templates

const Trigger = `CREATE OR REPLACE TRIGGER {{ .TriggerName | qi }}
    BEFORE UPDATE OR INSERT
    ON {{ .TableName | qi }}
    FOR EACH ROW
    EXECUTE PROCEDURE {{ .Name | qi }}();
`
