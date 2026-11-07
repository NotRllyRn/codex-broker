import type { Extension, SettingsSchema } from '../index';

export const settings = {
  description: 'Example configurable response header.',
  fields: [
    {key:'enabled', type:'boolean', label:'Add header', default:true},
    {key:'label', type:'text', label:'Header value', default:'Example', maxLength:80, visibleWhen:{key:'enabled', equals:true}},
    {key:'weight', type:'ratio', label:'Weight', default:0.5, step:0.1, group:'Advanced'},
    {key:'mode', type:'select', label:'Mode', default:'normal', options:[{value:'normal', label:'Normal'}, {value:'quiet', label:'Quiet'}]},
  ],
} satisfies SettingsSchema;

type Values = {enabled: boolean; label: string; weight: number; mode: string};
const extension: Extension = proxy => {
  let current = proxy.getSettings<Values>();
  proxy.on('settings_loaded', () => { current = proxy.getSettings<Values>(); });
  proxy.on('settings_changed', () => { current = proxy.getSettings<Values>(); proxy.log('info', 'Settings changed'); });
  proxy.on('response', event => current.enabled ? {headers:{...event.headers, 'X-Example':[current.label]}} : undefined);
};
export default extension;
