import overview from './overview'
import channels from './channels'
import accounts from './accounts'
import resources from './resources'
import ops from './ops'
import settings from './settings'
import smartRouting from './smartRouting'
import modelRpmRules from './modelRpmRules'
import audit from './audit'
import promptAudit from './promptAudit'
import plugins from './plugins'

export default {
  ...overview,
  ...channels,
  ...accounts,
  ...resources,
  ...ops,
  ...settings,
  ...smartRouting,
  ...modelRpmRules,
  ...audit,
  ...promptAudit,
  ...plugins,
}
