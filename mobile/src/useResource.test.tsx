import {act,create,type ReactTestRenderer} from 'react-test-renderer'
import {expect,it} from 'vitest'
import {useResource} from './useResource'
Object.assign(globalThis,{IS_REACT_ACT_ENVIRONMENT:true})
it('fences late success and failure after changing account or library seed',async()=>{
 let resource:ReturnType<typeof useResource<string>>
 let resolveOld!:(s:string)=>void
 let rejectOld!:(e:Error)=>void
 const old=()=>new Promise<string>((resolve,reject)=>{resolveOld=resolve;rejectOld=reject})
 function Probe({id,load}:{id:number;load:()=>Promise<string>}){resource=useResource(load,[id]);return null}
 let renderer!:ReactTestRenderer
 await act(async()=>{renderer=create(<Probe id={1} load={old}/> )})
 const formerRefresh=resource!.refresh
 await act(async()=>{renderer.update(<Probe id={2} load={async()=> 'current'}/> )})
 await act(async()=>{resolveOld('stale')})
 expect(resource!.data).toBe('current');expect(resource!.error).toBe('')
 await act(async()=>{await formerRefresh()})
 expect(resource!.data).toBe('current');expect(resource!.loading).toBe(false)
 await act(async()=>{renderer.update(<Probe id={3} load={old}/> )})
 await act(async()=>{renderer.update(<Probe id={4} load={async()=> 'new account'}/> )})
 await act(async()=>{rejectOld(new Error('stale failure'))})
 expect(resource!.data).toBe('new account');expect(resource!.error).toBe('')
 await act(async()=>{renderer.unmount()})
})
