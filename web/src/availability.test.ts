import { describe, expect, it } from 'vitest'
import { availabilityLabel, canAddFromAvailability, matchesAvailability } from './availability'
import type { Availability } from './api'
const fact=(patch: Partial<Availability>={}): Availability=>({catalog:'present',presence:'present',acquisition:'none',libraryItemId:1,copyId:0,expected:4,present:4,offloaded:0,...patch})
describe('availability decisions',()=>{
 it('never treats catalogue membership alone as a verified copy',()=>{
  const unknown=fact({presence:'unknown',present:0})
  expect(matchesAvailability(unknown,'present')).toBe(false)
  expect(matchesAvailability(unknown,'unknown')).toBe(true)
  expect(canAddFromAvailability(unknown)).toBe(false)
  expect(availabilityLabel(unknown)).toContain('unknown')
 })
 it('keeps partial and offloaded titles in the to-get view without offering duplicate Add',()=>{
  for(const presence of ['partial','offloaded','missing'] as const){const value=fact({presence,present:1});expect(matchesAvailability(value,'to-get')).toBe(true);expect(canAddFromAvailability(value)).toBe(false)}
  expect(availabilityLabel(fact({presence:'partial',present:1}))).toBe('1 of 4 aired episodes available')
 })
 it('requires affirmative absence before offering Add',()=>{
  expect(canAddFromAvailability(undefined)).toBe(false)
  expect(canAddFromAvailability(fact({catalog:'ambiguous'}))).toBe(false)
  expect(canAddFromAvailability(fact({catalog:'absent',presence:'unknown'}))).toBe(true)
 })
})
