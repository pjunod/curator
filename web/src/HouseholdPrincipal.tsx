import {createContext,useContext} from 'react'
export const HouseholdPrincipal=createContext('')
export const useHouseholdPrincipal=()=>useContext(HouseholdPrincipal)
