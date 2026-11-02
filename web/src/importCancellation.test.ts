import {afterEach,describe,expect,it,vi} from 'vitest'
import {cancelQueueImport,resolveImportReview} from './api'

afterEach(()=>vi.unstubAllGlobals())
describe('durable import control requests',()=>{
 it('preserves an epoch above JavaScript safe integer precision',async()=>{
  const fetch=vi.fn().mockResolvedValue(new Response(null,{status:204}));vi.stubGlobal('fetch',fetch)
  const identity={generationKey:'acquisition',epoch:'9007199254740993'}
  await cancelQueueImport(17,identity)
  expect(JSON.parse(fetch.mock.calls[0][1].body)).toEqual(identity)
 })
 it('sends exact reviewed IDs and explicit acknowledgement',async()=>{
  const fetch=vi.fn().mockResolvedValue(new Response('{}',{status:200}));vi.stubGlobal('fetch',fetch)
  await resolveImportReview(17,{generationKey:'acquisition',epoch:'3'},'retain_uncertain',['placement:one'],true)
  expect(JSON.parse(fetch.mock.calls[0][1].body)).toEqual({generationKey:'acquisition',epoch:'3',action:'retain_uncertain',ids:['placement:one'],acknowledged:true})
 })
})
